package runtime

import (
	"reflect"
	"testing"
)

func TestImagesToPrune(t *testing.T) {
	// Deliberately unsorted; created is a unix ts, higher = newer.
	refs := []imageRef{
		{ref: "piper/blog:100", created: 100},
		{ref: "piper/blog:400", created: 400},
		{ref: "piper/blog:200", created: 200},
		{ref: "piper/blog:300", created: 300},
	}
	cases := []struct {
		keep int
		want []string
	}{
		{keep: 0, want: []string{"piper/blog:400", "piper/blog:300", "piper/blog:200", "piper/blog:100"}}, // all
		{keep: 2, want: []string{"piper/blog:200", "piper/blog:100"}},                                     // drop older
		{keep: 4, want: nil}, // keep everything
		{keep: 9, want: nil}, // keep > count
	}
	for _, c := range cases {
		got := imagesToPrune(append([]imageRef(nil), refs...), c.keep)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("keep=%d: prune = %v, want %v", c.keep, got, c.want)
		}
	}
}
