// Package deploy orchestrates building, running, health-checking, and routing an app.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/getpiper/piper/internal/runtime"
	"github.com/getpiper/piper/internal/store"
)

type RouteSetter interface {
	UpsertRoute(host string, upstreamHostPort int) error
	RemoveRoute(host string) error
}

type Deployer struct {
	store   *store.Store
	runtime runtime.Runtime
	routes  RouteSetter
	baseDom string
}

func New(s *store.Store, rt runtime.Runtime, routes RouteSetter, baseDomain string) *Deployer {
	return &Deployer{store: s, runtime: rt, routes: routes, baseDom: baseDomain}
}

func (d *Deployer) hostFor(app string) string {
	return app + "." + d.baseDom
}

func (d *Deployer) hostForPreview(app string, pr int) string {
	return fmt.Sprintf("pr-%d-%s.%s", pr, app, d.baseDom)
}

// buildRunHealthy builds, runs, and health-checks app.  On failure it invokes
// recordFailed with whatever ids are known so the caller persists a "failed"
// record for the right (app, pr) row, then returns a wrapped error.
func (d *Deployer) buildRunHealthy(ctx context.Context, app store.App, srcDir string, recordFailed func(imageID, containerID string, hostPort int)) (runtime.BuildResult, runtime.RunResult, error) {
	tag := fmt.Sprintf("piper/%s:%d", app.Name, time.Now().Unix())
	build, err := d.runtime.Build(ctx, srcDir, tag)
	if err != nil {
		recordFailed(build.ImageID, "", 0)
		return build, runtime.RunResult{}, fmt.Errorf("build: %w", err)
	}
	run, err := d.runtime.Run(ctx, tag, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
	if err != nil {
		recordFailed(build.ImageID, run.ContainerID, run.HostPort)
		return build, run, fmt.Errorf("run: %w", err)
	}
	if err := d.runtime.WaitHealthy(ctx, run.HostPort); err != nil {
		_ = d.runtime.Stop(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort)
		return build, run, fmt.Errorf("health: %w", err)
	}
	return build, run, nil
}

func (d *Deployer) Deploy(ctx context.Context, appName, srcDir string) (store.Deployment, error) {
	app, err := d.store.GetApp(appName)
	if err != nil {
		return store.Deployment{}, err
	}
	previous, err := d.store.LatestRunning(appName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Deployment{}, err
	}

	build, run, err := d.buildRunHealthy(ctx, app, srcDir, func(img, cid string, hp int) {
		_, _ = d.store.CreateDeployment(appName, img, cid, hp, "failed")
	})
	if err != nil {
		return store.Deployment{}, err
	}

	dep, err := d.store.CreateDeployment(appName, build.ImageID, run.ContainerID, run.HostPort, "running")
	if err != nil {
		return store.Deployment{}, err
	}
	if err := d.routes.UpsertRoute(d.hostFor(appName), run.HostPort); err != nil {
		return store.Deployment{}, fmt.Errorf("route: %w", err)
	}
	if previous.ContainerID != "" && previous.ContainerID != run.ContainerID {
		_ = d.runtime.Stop(ctx, previous.ContainerID)
		_ = d.store.UpdateDeploymentStatus(previous.ID, "stopped")
	}
	return dep, nil
}

func (d *Deployer) DeployPreview(ctx context.Context, appName string, pr int, srcDir string) (store.Deployment, error) {
	app, err := d.store.GetApp(appName)
	if err != nil {
		return store.Deployment{}, err
	}
	previous, err := d.store.PreviewRunning(appName, pr)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Deployment{}, err
	}

	build, run, err := d.buildRunHealthy(ctx, app, srcDir, func(img, cid string, hp int) {
		_, _ = d.store.CreatePreviewDeployment(appName, pr, img, cid, hp, "failed")
	})
	if err != nil {
		return store.Deployment{}, err
	}

	dep, err := d.store.CreatePreviewDeployment(appName, pr, build.ImageID, run.ContainerID, run.HostPort, "running")
	if err != nil {
		return store.Deployment{}, err
	}
	if err := d.routes.UpsertRoute(d.hostForPreview(appName, pr), run.HostPort); err != nil {
		return store.Deployment{}, fmt.Errorf("route: %w", err)
	}
	if previous.ContainerID != "" && previous.ContainerID != run.ContainerID {
		_ = d.runtime.Stop(ctx, previous.ContainerID)
		_ = d.store.UpdateDeploymentStatus(previous.ID, "stopped")
	}
	return dep, nil
}

func (d *Deployer) TeardownPreview(ctx context.Context, appName string, pr int) error {
	dep, err := d.store.PreviewRunning(appName, pr)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = d.runtime.Stop(ctx, dep.ContainerID)
	if err := d.routes.RemoveRoute(d.hostForPreview(appName, pr)); err != nil {
		return fmt.Errorf("unroute: %w", err)
	}
	return d.store.UpdateDeploymentStatus(dep.ID, "stopped")
}
