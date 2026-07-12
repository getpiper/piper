// Package runtime builds and runs app containers.
package runtime

import (
	"context"
	"io"
)

// BuildResult carries the built image id and the build's plain-text log.
// Log is populated even when Build returns an error — that failing output is
// the whole point of capturing it.
type BuildResult struct {
	ImageID string
	Log     string
}

type RunResult struct {
	ContainerID string
	HostPort    int
}

// Runtime builds, runs, health-checks, and stops app containers.
type Runtime interface {
	Build(ctx context.Context, srcDir, imageTag string, progress io.Writer) (BuildResult, error)
	Run(ctx context.Context, imageTag string, containerPort int, env map[string]string) (RunResult, error)
	WaitHealthy(ctx context.Context, hostPort int) error
	Stop(ctx context.Context, containerID string) error
	Logs(ctx context.Context, containerID string) (io.ReadCloser, error)
	// PruneAppImages removes an app's built images (tagged piper/<app>:<ts>),
	// keeping the newest keep by creation time; keep<=0 removes them all.
	// Best-effort: images still in use by a running container are left in place.
	PruneAppImages(ctx context.Context, app string, keep int) error
}
