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

const deploymentCleanupTimeout = 5 * time.Second

type RouteSetter interface {
	UpsertRoute(host string, upstreamHostPort int) error
	RemoveRoute(host string) error
}

// HostnameRegistrar assigns a relay-terminated public hostname for an app over
// the tunnel. In terminated (free-tier) mode the Deployer routes that hostname
// instead of "<app>.<baseDom>". Implemented by *agent.TunnelClient; injected by
// piperd. Nil in LAN / BYO-domain mode.
type HostnameRegistrar interface {
	Register(app string) (string, error)
	Deregister(hostname string) error
}

type Deployer struct {
	store     *store.Store
	runtime   runtime.Runtime
	routes    RouteSetter
	baseDom   string
	registrar HostnameRegistrar
}

func New(s *store.Store, rt runtime.Runtime, routes RouteSetter, baseDomain string) *Deployer {
	return &Deployer{store: s, runtime: rt, routes: routes, baseDom: baseDomain}
}

// SetHostnameRegistrar puts the Deployer into relay-terminated mode: Deploy asks
// the registrar for each app's public hostname and routes that. Nil restores
// LAN/BYO behavior.
func (d *Deployer) SetHostnameRegistrar(r HostnameRegistrar) { d.registrar = r }

func (d *Deployer) hostFor(app string) string {
	return app + "." + d.baseDom
}

func (d *Deployer) hostForPreview(app string, pr int) string {
	return fmt.Sprintf("pr-%d-%s.%s", pr, app, d.baseDom)
}

func (d *Deployer) stopPartial(ctx context.Context, containerID string) {
	if containerID == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deploymentCleanupTimeout)
	defer cancel()
	_ = d.runtime.Stop(cleanupCtx, containerID)
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
		d.stopPartial(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort)
		return build, run, fmt.Errorf("run: %w", err)
	}
	if err := d.runtime.WaitHealthy(ctx, run.HostPort); err != nil {
		d.stopPartial(ctx, run.ContainerID)
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
		_, _ = d.store.CreateDeployment(appName, img, cid, hp, "failed", "")
	})
	if err != nil {
		return store.Deployment{}, err
	}

	dep, err := d.store.CreateDeployment(appName, build.ImageID, run.ContainerID, run.HostPort, "running", "")
	if err != nil {
		return store.Deployment{}, err
	}
	host := d.hostFor(appName)
	if d.registrar != nil {
		host, err = d.registrar.Register(appName)
		if err != nil {
			return store.Deployment{}, fmt.Errorf("register hostname: %w", err)
		}
	}
	if err := d.routes.UpsertRoute(host, run.HostPort); err != nil {
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
		_, _ = d.store.CreatePreviewDeployment(appName, pr, img, cid, hp, "failed", "")
	})
	if err != nil {
		return store.Deployment{}, err
	}

	dep, err := d.store.CreatePreviewDeployment(appName, pr, build.ImageID, run.ContainerID, run.HostPort, "running", "")
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
