package record

import (
	"bytes"
	"testing"
)

// benchPayload builds a realistic terminal stream: colored build-log-ish
// lines with occasional OSC 133 command boundaries, fed in PTY-sized chunks.
func benchPayload() []byte {
	var buf bytes.Buffer
	line := []byte("\x1b[32m ok \x1b[0m compiling module/foo/bar_test.go: no issues found (cached) 142ms\r\n")
	for cmd := 0; cmd < 50; cmd++ {
		buf.WriteString("\x1b]133;A\x07$ go test ./...\r\n")
		buf.WriteString("\x1b]6973;cmd=Z28gdGVzdCAuLy4uLg==\x07")
		buf.WriteString("\x1b]133;C\x07")
		for i := 0; i < 200; i++ {
			buf.Write(line)
		}
		buf.WriteString("\x1b]133;D;0\x07")
	}
	return buf.Bytes()
}

func BenchmarkParserFeed(b *testing.B) {
	payload := benchPayload()
	ev := Events{
		CmdText:  func(string) {},
		OutStart: func() {},
		OutEnd:   func(int, bool) {},
		Cwd:      func(string) {},
		Output:   func([]byte) {},
		Toggle:   func(bool) {},
	}
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := NewParser(ev)
		// feed in 4 KiB chunks, like PTY reads
		for off := 0; off < len(payload); off += 4096 {
			end := off + 4096
			if end > len(payload) {
				end = len(payload)
			}
			p.Feed(payload[off:end])
		}
	}
}

func BenchmarkCapBufWrite(b *testing.B) {
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 256) // 4 KiB
	b.SetBytes(int64(len(chunk)))
	cb := newCapBuf(2<<20, 2<<20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cb.Write(chunk)
	}
}
