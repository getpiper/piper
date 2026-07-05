package source_test

import (
	"testing"

	"github.com/getpiper/piper/internal/source"
)

func TestKindString(t *testing.T) {
	cases := map[source.Kind]string{
		source.KindPush:  "push",
		source.KindPing:  "ping",
		source.KindOther: "other",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

func TestStatusInactiveDistinct(t *testing.T) {
	all := []source.Status{
		source.StatusPending, source.StatusSuccess,
		source.StatusFailure, source.StatusInactive,
	}
	seen := map[source.Status]bool{}
	for _, s := range all {
		if seen[s] {
			t.Fatalf("duplicate status value %d", s)
		}
		seen[s] = true
	}
}
