//go:build windows

// conptydebug: isolate ConPTY plumbing variants on a real Windows
// runner. Temporary debugging tool for the windows branch.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	xconpty "github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

func main() {
	fmt.Println("=== variant A: our plumbing, explicit env block ===")
	variantOurs(true)
	fmt.Println("=== variant B: our plumbing, inherited env (nil) ===")
	variantOurs(false)
	fmt.Println("=== variant C: UserExistsError/conpty ===")
	variantLib()
}

func buildEnvBlock(env []string) []uint16 {
	sorted := append([]string(nil), env...)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.ToUpper(sorted[i]) < strings.ToUpper(sorted[j])
	})
	var b []uint16
	for _, kv := range sorted {
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	b = append(b, 0)
	return b
}

func variantOurs(withEnv bool) {
	exe, err := exec.LookPath("cmd.exe")
	if err != nil {
		fmt.Println("lookpath:", err)
		return
	}
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		fmt.Println("pipe1:", err)
		return
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		fmt.Println("pipe2:", err)
		return
	}
	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(windows.Coord{X: 100, Y: 30}, inR, outW, 0, &hpc); err != nil {
		fmt.Println("createpc:", err)
		return
	}
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		fmt.Println("attrs:", err)
		return
	}
	defer attrs.Delete()
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		*(*unsafe.Pointer)(unsafe.Pointer(&hpc)), unsafe.Sizeof(hpc)); err != nil {
		fmt.Println("update:", err)
		return
	}
	si := new(windows.StartupInfoEx)
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.ProcThreadAttributeList = attrs.List()
	si.Flags = windows.STARTF_USESTDHANDLES

	argv, _ := windows.UTF16PtrFromString(windows.EscapeArg(exe))
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT)
	var envp *uint16
	if withEnv {
		block := buildEnvBlock(append(os.Environ(), "BKS_DEBUG=1"))
		envp = &block[0]
		flags |= windows.CREATE_UNICODE_ENVIRONMENT
	}
	var pi windows.ProcessInformation
	err = windows.CreateProcess(nil, argv, nil, nil, false, flags, envp, nil, &si.StartupInfo, &pi)
	if err != nil {
		fmt.Println("createprocess:", err)
		return
	}
	windows.CloseHandle(pi.Thread)

	in := os.NewFile(uintptr(inW), "in")
	out := os.NewFile(uintptr(outR), "out")
	collect := make(chan byte, 1<<20)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := out.Read(buf)
			for _, b := range buf[:n] {
				collect <- b
			}
			if err != nil {
				close(collect)
				return
			}
		}
	}()

	drain := func(d time.Duration) string {
		var sb strings.Builder
		deadline := time.After(d)
		for {
			select {
			case b, ok := <-collect:
				if !ok {
					return sb.String()
				}
				sb.WriteByte(b)
			case <-deadline:
				return sb.String()
			}
		}
	}

	fmt.Printf("  startup: %q\n", drain(5*time.Second))
	n, werr := in.Write([]byte("echo marco-polo\r"))
	fmt.Printf("  wrote %d err=%v\n", n, werr)
	got := drain(5 * time.Second)
	fmt.Printf("  after-echo: %q\n", got)
	fmt.Printf("  RESULT: echo-visible=%v\n", strings.Contains(got, "marco-polo"))
	in.Write([]byte("exit\r"))
	time.Sleep(2 * time.Second)
	windows.ClosePseudoConsole(hpc)
	windows.CloseHandle(pi.Process)
	fmt.Printf("  tail: %q\n", drain(2*time.Second))
}

func variantLib() {
	cpty, err := xconpty.Start("cmd.exe")
	if err != nil {
		fmt.Println("lib start:", err)
		return
	}
	defer cpty.Close()
	collect := make(chan byte, 1<<20)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := cpty.Read(buf)
			for _, b := range buf[:n] {
				collect <- b
			}
			if err != nil {
				close(collect)
				return
			}
		}
	}()
	drain := func(d time.Duration) string {
		var sb strings.Builder
		deadline := time.After(d)
		for {
			select {
			case b, ok := <-collect:
				if !ok {
					return sb.String()
				}
				sb.WriteByte(b)
			case <-deadline:
				return sb.String()
			}
		}
	}
	fmt.Printf("  startup: %q\n", drain(5*time.Second))
	n, werr := cpty.Write([]byte("echo marco-polo\r"))
	fmt.Printf("  wrote %d err=%v\n", n, werr)
	got := drain(5 * time.Second)
	fmt.Printf("  after-echo: %q\n", got)
	fmt.Printf("  RESULT: echo-visible=%v\n", strings.Contains(got, "marco-polo"))
	cpty.Write([]byte("exit\r"))
	time.Sleep(2 * time.Second)
}
