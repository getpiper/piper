package runtime

import (
	"context"
	"io"
	"strings"
)

// FakeRuntime is an in-memory Runtime for unit tests.
type FakeRuntime struct {
	BuildResultVal  BuildResult
	BuildOutput     string // written to progress on Build, simulating live output
	BuildErr        error
	RunResultVal    RunResult
	RunErr          error
	HealthErr       error
	LogsVal         string
	LogsErr         error
	Stopped         []string
	StopContextErrs []error
	Pruned          []PruneCall
	PruneErr        error
}

// PruneCall records one PruneAppImages invocation for assertions.
type PruneCall struct {
	App  string
	Keep int
}

func (f *FakeRuntime) Build(_ context.Context, _, _ string, progress io.Writer) (BuildResult, error) {
	if progress != nil && f.BuildOutput != "" {
		_, _ = io.WriteString(progress, f.BuildOutput)
	}
	return f.BuildResultVal, f.BuildErr
}

func (f *FakeRuntime) Run(context.Context, string, int, map[string]string) (RunResult, error) {
	return f.RunResultVal, f.RunErr
}

func (f *FakeRuntime) WaitHealthy(context.Context, int) error { return f.HealthErr }

func (f *FakeRuntime) Stop(ctx context.Context, id string) error {
	f.Stopped = append(f.Stopped, id)
	f.StopContextErrs = append(f.StopContextErrs, ctx.Err())
	return nil
}

func (f *FakeRuntime) PruneAppImages(_ context.Context, app string, keep int) error {
	f.Pruned = append(f.Pruned, PruneCall{App: app, Keep: keep})
	return f.PruneErr
}

func (f *FakeRuntime) Logs(context.Context, string) (io.ReadCloser, error) {
	if f.LogsErr != nil {
		return nil, f.LogsErr
	}
	return io.NopCloser(strings.NewReader(f.LogsVal)), nil
}
