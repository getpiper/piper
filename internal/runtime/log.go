package runtime

// LogCap bounds every captured deployment log blob so a pathological build
// can't balloon memory on a Pi-class box.
const LogCap = 1 << 20 // 1 MiB

// TailBuffer is an io.Writer that keeps only the last LogCap bytes written.
// Errors live at the end of a log, so the tail is the part worth keeping.
type TailBuffer struct {
	buf       []byte
	truncated bool
}

func (t *TailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > LogCap {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-LogCap:]...)
		t.truncated = true
	}
	return len(p), nil
}

// String returns the captured tail, prefixed with a truncation marker when
// earlier output was dropped.
func (t *TailBuffer) String() string {
	if t.truncated {
		return "[log truncated]\n" + string(t.buf)
	}
	return string(t.buf)
}
