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

func (d *Deployer) Deploy(ctx context.Context, appName, srcDir string) (store.Deployment, error) {
	app, err := d.store.GetApp(appName)
	if err != nil {
		return store.Deployment{}, err
	}
	previous, err := d.store.LatestRunning(appName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Deployment{}, err
	}

	tag := fmt.Sprintf("piper/%s:%d", appName, time.Now().Unix())
	build, err := d.runtime.Build(ctx, srcDir, tag)
	if err != nil {
		_, _ = d.store.CreateDeployment(appName, build.ImageID, "", 0, "failed")
		return store.Deployment{}, fmt.Errorf("build: %w", err)
	}
	run, err := d.runtime.Run(ctx, tag, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
	if err != nil {
		_, _ = d.store.CreateDeployment(appName, build.ImageID, run.ContainerID, run.HostPort, "failed")
		return store.Deployment{}, fmt.Errorf("run: %w", err)
	}
	if err := d.runtime.WaitHealthy(ctx, run.HostPort); err != nil {
		_ = d.runtime.Stop(ctx, run.ContainerID)
		_, _ = d.store.CreateDeployment(appName, build.ImageID, run.ContainerID, run.HostPort, "failed")
		return store.Deployment{}, fmt.Errorf("health: %w", err)
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
