package telemetry

type BodyCapture struct {
	buf   []byte
	total int64
}

func NewBodyCapture() *BodyCapture { return &BodyCapture{} }

func (c *BodyCapture) Write(p []byte) (int, error) {
	c.total += int64(len(p))
	if room := MaxBodyBytes - len(c.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
	}
	return len(p), nil
}

func (c *BodyCapture) String() string { return string(c.buf) }

func (c *BodyCapture) Truncated() bool { return c.total > int64(len(c.buf)) }

func (c *BodyCapture) Total() int64 { return c.total }

func CaptureBody(b []byte) (body string, truncated bool) {
	if len(b) > MaxBodyBytes {
		return string(b[:MaxBodyBytes]), true
	}
	return string(b), false
}

func CaptureString(s string) (body string, truncated bool) {
	if len(s) > MaxBodyBytes {
		return s[:MaxBodyBytes], true
	}
	return s, false
}
