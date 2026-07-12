package runtime

import (
	"bytes"
	"context"
	"testing"
)

func TestFakeBuildWritesProgress(t *testing.T) {
	f := &FakeRuntime{
		BuildResultVal: BuildResult{ImageID: "img-1"},
		BuildOutput:    "Step 1/2 : FROM alpine\nStep 2/2 : CMD sh\n",
	}
	var progress bytes.Buffer
	got, err := f.Build(context.Background(), "/src", "tag", &progress)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.ImageID != "img-1" {
		t.Fatalf("ImageID = %q", got.ImageID)
	}
	if progress.String() != "Step 1/2 : FROM alpine\nStep 2/2 : CMD sh\n" {
		t.Fatalf("progress = %q", progress.String())
	}
}

func TestFakeBuildNilProgressIsSafe(t *testing.T) {
	f := &FakeRuntime{BuildResultVal: BuildResult{ImageID: "img-1"}, BuildOutput: "x"}
	if _, err := f.Build(context.Background(), "/src", "tag", nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
}
