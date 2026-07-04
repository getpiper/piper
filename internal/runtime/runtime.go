// Package runtime builds and runs app containers.
package runtime

import (
	"context"
	"io"
)

type BuildResult struct{ ImageID string }

type RunResult struct {
	ContainerID string
	HostPort    int
}

// Runtime builds, runs, health-checks, and stops app containers.
type Runtime interface {
	Build(ctx context.Context, srcDir, imageTag string) (BuildResult, error)
	Run(ctx context.Context, imageTag string, containerPort int, env map[string]string) (RunResult, error)
	WaitHealthy(ctx context.Context, hostPort int) error
	Stop(ctx context.Context, containerID string) error
	Logs(ctx context.Context, containerID string) (io.ReadCloser, error)
}
