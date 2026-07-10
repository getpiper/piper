package runtime

import (
	"context"
	"io"
	"strings"
)

// FakeRuntime is an in-memory Runtime for unit tests.
type FakeRuntime struct {
	BuildResultVal  BuildResult
	BuildErr        error
	RunResultVal    RunResult
	RunErr          error
	HealthErr       error
	LogsVal         string
	LogsErr         error
	Stopped         []string
	StopContextErrs []error
}

func (f *FakeRuntime) Build(context.Context, string, string) (BuildResult, error) {
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

func (f *FakeRuntime) Logs(context.Context, string) (io.ReadCloser, error) {
	if f.LogsErr != nil {
		return nil, f.LogsErr
	}
	return io.NopCloser(strings.NewReader(f.LogsVal)), nil
}
