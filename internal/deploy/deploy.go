// Package deploy orchestrates building, running, health-checking, and routing an app.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/getpiper/piper/internal/runtime"
	"github.com/getpiper/piper/internal/store"
)

const deploymentCleanupTimeout = 5 * time.Second

type RouteSetter interface {
	UpsertRoute(host string, upstreamHostPort int) error
	UpsertRouteTLS(host string, upstreamHostPort int) error
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

// buildRunHealthy builds, runs, and health-checks app, capturing one
// tail-capped log blob (build output, plus container output when the run or
// health check fails). On failure it invokes recordFailed with whatever ids
// and log are known so the caller persists a "failed" record for the right
// (app, pr) row, then returns a wrapped error.
func (d *Deployer) buildRunHealthy(ctx context.Context, app store.App, srcDir string, recordFailed func(imageID, containerID string, hostPort int, logs string)) (runtime.BuildResult, runtime.RunResult, string, error) {
	tag := fmt.Sprintf("piper/%s:%d", app.Name, time.Now().Unix())
	var log runtime.TailBuffer
	build, err := d.runtime.Build(ctx, srcDir, tag)
	_, _ = io.WriteString(&log, build.Log)
	if err != nil {
		_, _ = io.WriteString(&log, "\nerror: "+err.Error()+"\n")
		recordFailed(build.ImageID, "", 0, log.String())
		return build, runtime.RunResult{}, log.String(), fmt.Errorf("build: %w", err)
	}
	run, err := d.runtime.Run(ctx, tag, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
	if err != nil {
		d.appendContainerOutput(ctx, &log, run.ContainerID)
		_, _ = io.WriteString(&log, "\nerror: "+err.Error()+"\n")
		d.stopPartial(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort, log.String())
		return build, run, log.String(), fmt.Errorf("run: %w", err)
	}
	if err := d.runtime.WaitHealthy(ctx, run.HostPort); err != nil {
		d.appendContainerOutput(ctx, &log, run.ContainerID)
		_, _ = io.WriteString(&log, "\nerror: "+err.Error()+"\n")
		d.stopPartial(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort, log.String())
		return build, run, log.String(), fmt.Errorf("health: %w", err)
	}
	return build, run, log.String(), nil
}

// appendContainerOutput best-effort appends the container's stdout/stderr to
// log; it must run before stopPartial removes the container. A fetch failure
// never masks the deploy error. Detached context so a cancelled deploy can
// still capture (same rationale as stopPartial).
func (d *Deployer) appendContainerOutput(ctx context.Context, log io.Writer, containerID string) {
	if containerID == "" {
		return
	}
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deploymentCleanupTimeout)
	defer cancel()
	rc, err := d.runtime.Logs(logCtx, containerID)
	if err != nil {
		return
	}
	defer rc.Close()
	_, _ = io.WriteString(log, "\n--- container output ---\n")
	_, _ = io.Copy(log, rc)
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

	build, run, logs, err := d.buildRunHealthy(ctx, app, srcDir, func(img, cid string, hp int, logs string) {
		_, _ = d.store.CreateDeployment(appName, img, cid, hp, "failed", logs)
	})
	if err != nil {
		return store.Deployment{}, err
	}

	dep, err := d.store.CreateDeployment(appName, build.ImageID, run.ContainerID, run.HostPort, "running", logs)
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
	// An active BYO custom domain (#102) serves the app at <app>.<custom> over
	// the box-terminated :443 alongside the primary host.
	if dc, err := d.store.GetDomainConfig(); err == nil && dc.Status == "active" {
		if err := d.routes.UpsertRouteTLS(appName+"."+dc.Domain, run.HostPort); err != nil {
			return store.Deployment{}, fmt.Errorf("route custom domain: %w", err)
		}
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

	build, run, logs, err := d.buildRunHealthy(ctx, app, srcDir, func(img, cid string, hp int, logs string) {
		_, _ = d.store.CreatePreviewDeployment(appName, pr, img, cid, hp, "failed", logs)
	})
	if err != nil {
		return store.Deployment{}, err
	}

	dep, err := d.store.CreatePreviewDeployment(appName, pr, build.ImageID, run.ContainerID, run.HostPort, "running", logs)
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

// primaryHost resolves the app's routed production host: the relay-assigned
// name in registrar mode (recovered via the idempotent Register), else
// <app>.<baseDom>. ok is false when the relay is unreachable and the
// hostname can't be recovered — callers skip route work best-effort.
func (d *Deployer) primaryHost(appName string) (string, bool) {
	if d.registrar == nil {
		return d.hostFor(appName), true
	}
	host, err := d.registrar.Register(appName)
	return host, err == nil
}

// removeCustomDomainRoute drops <app>.<custom> when a BYO custom domain is
// active; without one it's a no-op (mirrors the upsert in Deploy).
func (d *Deployer) removeCustomDomainRoute(appName string) error {
	dc, err := d.store.GetDomainConfig()
	if err != nil || dc.Status != "active" {
		return nil
	}
	if err := d.routes.RemoveRoute(appName + "." + dc.Domain); err != nil {
		return fmt.Errorf("unroute custom domain: %w", err)
	}
	return nil
}

// Stop retires the app's running production container: stop it, drop its
// routes, mark the deployment "stopped". The app and its history remain.
// Nothing running is a no-op; previews are untouched. The relay keeps the
// app's hostname registration (Deregister is delete-only).
func (d *Deployer) Stop(ctx context.Context, appName string) error {
	if _, err := d.store.GetApp(appName); err != nil {
		return err
	}
	dep, err := d.store.LatestRunning(appName)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = d.runtime.Stop(ctx, dep.ContainerID)
	if host, ok := d.primaryHost(appName); ok {
		if err := d.routes.RemoveRoute(host); err != nil {
			return fmt.Errorf("unroute: %w", err)
		}
	}
	if err := d.removeCustomDomainRoute(appName); err != nil {
		return err
	}
	return d.store.UpdateDeploymentStatus(dep.ID, "stopped")
}
