package record

import (
	"bytes"
	"fmt"
	"testing"
)

// eventLog flattens every parser callback into a comparable trace.
type eventLog struct {
	trace  []string
	output bytes.Buffer
}

func (l *eventLog) events() Events {
	return Events{
		CmdText:  func(cmd string) { l.trace = append(l.trace, "cmd:"+cmd) },
		OutStart: func() { l.trace = append(l.trace, "start") },
		OutEnd: func(code int, ok bool) {
			l.trace = append(l.trace, fmt.Sprintf("end:%d:%v", code, ok))
		},
		Cwd:    func(p string) { l.trace = append(l.trace, "cwd:"+p) },
		Toggle: func(on bool) { l.trace = append(l.trace, fmt.Sprintf("rec:%v", on)) },
		Output: func(b []byte) { l.output.Write(b) },
	}
}

func runParser(data []byte, chunks [][]byte) *eventLog {
	l := &eventLog{}
	p := NewParser(l.events())
	if chunks == nil {
		p.Feed(data)
	} else {
		for _, c := range chunks {
			p.Feed(c)
		}
	}
	return l
}

// splitBytes deterministically splits data into chunks using seed:
// chunk lengths cycle through 1..(seed%7+1), plus a few length-0 feeds.
func splitBytes(data []byte, seed uint8) [][]byte {
	var chunks [][]byte
	step := int(seed%7) + 1
	for i := 0; i < len(data); {
		n := (i+step)%step + 1 // 1..step
		if i+n > len(data) {
			n = len(data) - i
		}
		chunks = append(chunks, data[i:i+n])
		if seed%3 == 0 {
			chunks = append(chunks, nil) // empty feed must be harmless
		}
		i += n
	}
	return chunks
}

// FuzzParserChunking checks two invariants on arbitrary byte streams:
//  1. the parser never panics;
//  2. the observed event trace and captured output are identical no
//     matter how the stream is split across Feed calls (a real PTY
//     delivers bytes at arbitrary boundaries, including mid-escape).
func FuzzParserChunking(f *testing.F) {
	seeds := [][]byte{
		[]byte("plain text\n"),
		[]byte("\x1b]133;C\x07hello\n\x1b]133;D;0\x07"),
		[]byte("\x1b]133;C\x1b\\out\x1b]133;D;1\x1b\\"),
		[]byte("\x1b]6973;cmd=bHM=\x07\x1b]133;C\x07x\x1b]133;D;0\x07"),
		[]byte("\x1b]7;file://host/tmp/a%20b\x07"),
		[]byte("\x1b]133;C\x07\x1b[?1049hALT\x1b[?1049lvis\x1b]133;D\x07"),
		[]byte("\x1b]6973;rec=off\x07\x1b]133;C\x07hidden\x1b]133;D;0\x07\x1b]6973;rec=on\x07"),
		[]byte("\x1b]133;C\x07col\x1b[31mred\x1b[0m\x1b]8;;http://x\x07link\x1b]8;;\x07\x1b]133;D;0\x07"),
		[]byte("\x1b]133;D;0\x07\x1b]133;D\x07"),        // D without C
		[]byte("\x1b]133;C\x07trunc..."),                // unterminated capture
		[]byte("\x1b]133;C\x07a\x1bZb\x1b]133;D;0\x07"), // odd escape inside output
		[]byte("\x1b]999;unknown\x07\x1b]133;C\x07\x1b]999;inner\x07q\x1b]133;D;0\x07"),
		{0x1b}, {0x1b, ']'}, {0x1b, '['}, // dangling starts
	}
	for _, s := range seeds {
		for seed := uint8(0); seed < 4; seed++ {
			f.Add(s, seed)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, seed uint8) {
		whole := runParser(data, nil)
		split := runParser(data, splitBytes(data, seed))

		if fmt.Sprint(whole.trace) != fmt.Sprint(split.trace) {
			t.Fatalf("event trace differs by chunking:\nwhole: %q\nsplit: %q\ninput: %q",
				whole.trace, split.trace, data)
		}
		if !bytes.Equal(whole.output.Bytes(), split.output.Bytes()) {
			t.Fatalf("captured output differs by chunking:\nwhole: %q\nsplit: %q\ninput: %q",
				whole.output.Bytes(), split.output.Bytes(), data)
		}
	})
}
