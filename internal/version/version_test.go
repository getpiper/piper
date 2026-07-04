package version

import "testing"

func TestStringNotEmpty(t *testing.T) {
	if String() == "" {
		t.Fatal("version.String() returned empty string")
	}
}
