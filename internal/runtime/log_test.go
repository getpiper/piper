package runtime

import (
	"strings"
	"testing"
)

func TestTailBufferPassthroughUnderCap(t *testing.T) {
	var b TailBuffer
	if _, err := b.Write([]byte("hello\nworld\n")); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "hello\nworld\n" {
		t.Errorf("String() = %q, want passthrough", got)
	}
}

func TestTailBufferKeepsTailAndMarksTruncation(t *testing.T) {
	var b TailBuffer
	if _, err := b.Write([]byte(strings.Repeat("x", LogCap))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("THE END")); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if !strings.HasPrefix(got, "[log truncated]\n") {
		t.Errorf("missing truncation marker: %q...", got[:32])
	}
	if !strings.HasSuffix(got, "THE END") {
		t.Error("tail was not kept")
	}
	if len(got) > LogCap+len("[log truncated]\n") {
		t.Errorf("len = %d, want <= cap+marker", len(got))
	}
}
