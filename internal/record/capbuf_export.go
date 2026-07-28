package record

// CapBuf is the bounded stream buffer used for command output capture:
// it keeps the first headCap and last tailCap bytes of everything
// written, inserting a "[N bytes truncated]" marker between them when
// the stream overflows. Exported for use by `backscroll exec`, which
// records one-shot commands without a PTY parser in the loop.
type CapBuf = capBuf

// NewCapBuf returns a CapBuf with the given head/tail byte caps.
func NewCapBuf(headCap, tailCap int) *CapBuf { return newCapBuf(headCap, tailCap) }

// Total returns the total number of bytes written so far.
func (c *capBuf) Total() int64 { return c.total }
