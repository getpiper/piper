// Package deploy orchestrates building, running, health-checking, and routing an app.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/getpiper/piper/internal/runtime"
	"github.com/getpiper/piper/internal/store"
)

const deploymentCleanupTimeout = 5 * time.Second

// logFlushInterval bounds how often a running build's growing log is persisted
// to its deployment row, so a slow build's output reaches pollers (the CLI and
// dashboard) without a store write per line.
const logFlushInterval = time.Second

// logSink is the progress io.Writer handed to the build: it accumulates output
// in a tail-capped buffer and flushes the whole buffer to the deployment's log
// column at most once per logFlushInterval. Written from a single goroutine
// (the deploy), so it needs no locking. The authoritative final log is written
// separately by FinalizeDeployment.
type logSink struct {
	buf       runtime.TailBuffer
	store     *store.Store
	id        string
	lastFlush time.Time
}

func (ls *logSink) Write(p []byte) (int, error) {
	n, err := ls.buf.Write(p)
	if time.Since(ls.lastFlush) >= logFlushInterval {
		_ = ls.store.UpdateDeploymentLogs(ls.id, ls.buf.String())
		ls.lastFlush = time.Now()
	}
	return n, err
}

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

	mu    sync.Mutex             // guards locks
	locks map[string]*sync.Mutex // per-app serialization of mutating ops
}

func New(s *store.Store, rt runtime.Runtime, routes RouteSetter, baseDomain string) *Deployer {
	return &Deployer{store: s, runtime: rt, routes: routes, baseDom: baseDomain, locks: map[string]*sync.Mutex{}}
}

// lockApp serializes every mutating operation for one app (deploy, preview,
// stop, delete) so they can't interleave: two concurrent deploys racing on
// routing/finalize, a deploy racing a delete into an orphan container + a
// resurrected route, or two same-second builds colliding on the image tag.
// The webhook path has its own appLock; this closes the API + delete paths.
// Returns the unlock func so callers can `defer d.lockApp(name)()`. #159, #121.
func (d *Deployer) lockApp(name string) func() {
	d.mu.Lock()
	m := d.locks[name]
	if m == nil {
		m = &sync.Mutex{}
		d.locks[name] = m
	}
	d.mu.Unlock()
	m.Lock()
	return m.Unlock
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
// health check fails). When progress is non-nil (the production Finish path),
// stage-transition banners ("→ building image", etc.) and the build's live
// output are written to it as they happen, in addition to the returned log;
// previews pass nil and see no live output. On failure it invokes recordFailed
// with whatever ids and log are known so the caller persists a "failed" record
// for the right (app, pr) row, then returns a wrapped error.
func (d *Deployer) buildRunHealthy(ctx context.Context, app store.App, srcDir string, progress io.Writer, recordFailed func(imageID, containerID string, hostPort int, logs string)) (runtime.BuildResult, runtime.RunResult, string, error) {
	tag := fmt.Sprintf("piper/%s:%d", app.Name, time.Now().Unix())
	var log runtime.TailBuffer
	out := io.Writer(&log)
	if progress != nil {
		out = io.MultiWriter(&log, progress)
	}
	_, _ = io.WriteString(out, "→ building image\n")
	build, err := d.runtime.Build(ctx, srcDir, tag, progress)
	_, _ = io.WriteString(&log, build.Log)
	if err != nil {
		_, _ = io.WriteString(&log, "\nerror: "+err.Error()+"\n")
		recordFailed(build.ImageID, "", 0, log.String())
		return build, runtime.RunResult{}, log.String(), fmt.Errorf("build: %w", err)
	}
	_, _ = io.WriteString(out, "→ starting container\n")
	run, err := d.runtime.Run(ctx, tag, app.Port, map[string]string{"PORT": fmt.Sprint(app.Port)})
	if err != nil {
		d.appendContainerOutput(ctx, &log, run.ContainerID)
		_, _ = io.WriteString(&log, "\nerror: "+err.Error()+"\n")
		d.stopPartial(ctx, run.ContainerID)
		recordFailed(build.ImageID, run.ContainerID, run.HostPort, log.String())
		return build, run, log.String(), fmt.Errorf("run: %w", err)
	}
	_, _ = io.WriteString(out, "→ health-checking\n")
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

// Begin opens a building deployment row for appName and returns it; its id is
// what an async caller (the deploy API) hands back before the build finishes.
func (d *Deployer) Begin(appName string) (store.Deployment, error) {
	if _, err := d.store.GetApp(appName); err != nil {
		return store.Deployment{}, err
	}
	return d.store.CreateDeployment(appName, "", "", 0, "building", "")
}

// Finish builds, runs, health-checks, and routes dep's app from srcDir,
// streaming the build's output into dep's log and finalizing the row
// running/failed. On build/run/health failure it finalizes the same row failed.
// It serializes per app against other deploys/deletes (see lockApp).
func (d *Deployer) Finish(ctx context.Context, dep store.Deployment, srcDir string) error {
	defer d.lockApp(dep.App)()
	return d.finish(ctx, dep, srcDir)
}

// finish is Finish without the per-app lock, for callers that already hold it
// (Deploy). Never call it directly from an unlocked path.
func (d *Deployer) finish(ctx context.Context, dep store.Deployment, srcDir string) (retErr error) {
	app, err := d.store.GetApp(dep.App)
	if err != nil {
		return err
	}
	previous, err := d.store.LatestRunning(dep.App)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	sink := &logSink{store: d.store, id: dep.ID}
	build, run, logs, err := d.buildRunHealthy(ctx, app, srcDir, sink, func(img, cid string, hp int, logs string) {
		_ = d.store.FinalizeDeployment(dep.ID, img, cid, hp, "failed", logs)
	})
	if err != nil {
		return err
	}

	// A container is now running. From here a panic in routing/finalize (a nil
	// registrar, a Caddy client bug) would orphan it, so recover, stop it, and
	// finalize the row "failed" rather than leaking it (#162). The deploy layer
	// owns this cleanup because only it knows the container id — the api
	// goroutine's recover can't.
	defer func() {
		if r := recover(); r != nil {
			d.stopPartial(ctx, run.ContainerID)
			logs += fmt.Sprintf("\ndeploy panicked: %v\n", r)
			_ = d.store.FinalizeDeployment(dep.ID, build.ImageID, run.ContainerID, run.HostPort, "failed", logs)
			retErr = fmt.Errorf("deploy panicked: %v", r)
		}
	}()

	// Route BEFORE marking the row "running": if routing fails, the app isn't
	// reachable yet, so the row must finalize "failed" rather than report a
	// success the CLI/dashboard would trust.
	host := d.hostFor(dep.App)
	if d.registrar != nil {
		host, err = d.registrar.Register(dep.App)
		if err != nil {
			return d.failFinish(ctx, dep.ID, build.ImageID, run.ContainerID, run.HostPort, logs, fmt.Errorf("register hostname: %w", err))
		}
	}
	if err := d.routes.UpsertRoute(host, run.HostPort); err != nil {
		return d.failFinish(ctx, dep.ID, build.ImageID, run.ContainerID, run.HostPort, logs, fmt.Errorf("route: %w", err))
	}
	// Record the host the app is now served on so the apps API (#100) and deploy
	// response (#93) can report the real URL, not a guessed one.
	if err := d.store.SetAppHostname(dep.App, host); err != nil {
		return d.failFinish(ctx, dep.ID, build.ImageID, run.ContainerID, run.HostPort, logs, fmt.Errorf("record hostname: %w", err))
	}
	// An active BYO custom domain (#102) serves the app at <app>.<custom> over
	// the box-terminated :443 alongside the primary host. This is a secondary
	// hostname: the primary route is already live and the domain manager re-arms
	// custom-domain routes on renewal/resume, so a transient Caddy error here is
	// logged and skipped rather than failing an otherwise-successful deploy (#115).
	if dc, err := d.store.GetDomainConfig(); err == nil && dc.Status == "active" {
		host := dep.App + "." + dc.Domain
		if err := d.routes.UpsertRouteTLS(host, run.HostPort); err != nil {
			log.Printf("deploy %s: custom-domain route %s (deploy still succeeded on primary): %v", dep.App, host, err)
		}
	}

	if err := d.store.FinalizeDeployment(dep.ID, build.ImageID, run.ContainerID, run.HostPort, "running", logs); err != nil {
		return err
	}
	if previous.ContainerID != "" && previous.ContainerID != run.ContainerID {
		_ = d.runtime.Stop(ctx, previous.ContainerID)
		_ = d.store.UpdateDeploymentStatus(previous.ID, "stopped")
	}
	return nil
}

// failFinish finalizes dep's row "failed" after a routing error, appending the
// error to logs, and returns the wrapped error unchanged. It best-effort stops
// the just-started container: the row is unreachable, so leaving the container
// running would orphan it (#162).
func (d *Deployer) failFinish(ctx context.Context, id, imageID, containerID string, hostPort int, logs string, wrapped error) error {
	d.stopPartial(ctx, containerID)
	logs += "\nerror: " + wrapped.Error() + "\n"
	_ = d.store.FinalizeDeployment(id, imageID, containerID, hostPort, "failed", logs)
	return wrapped
}

// Deploy is the synchronous Begin+Finish used by the webhook path; it returns
// the finalized deployment.
func (d *Deployer) Deploy(ctx context.Context, appName, srcDir string) (store.Deployment, error) {
	defer d.lockApp(appName)()
	dep, err := d.Begin(appName)
	if err != nil {
		return store.Deployment{}, err
	}
	if err := d.finish(ctx, dep, srcDir); err != nil {
		return store.Deployment{}, err
	}
	return d.store.LatestDeployment(appName)
}

func (d *Deployer) DeployPreview(ctx context.Context, appName string, pr int, srcDir string) (store.Deployment, error) {
	defer d.lockApp(appName)()
	app, err := d.store.GetApp(appName)
	if err != nil {
		return store.Deployment{}, err
	}
	previous, err := d.store.PreviewRunning(appName, pr)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Deployment{}, err
	}

	build, run, logs, err := d.buildRunHealthy(ctx, app, srcDir, nil, func(img, cid string, hp int, logs string) {
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
	defer d.lockApp(appName)()
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
	defer d.lockApp(appName)()
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

// Delete tears the app down completely: stops every running deployment
// (production and previews), drops all its routes, releases the relay
// hostname, and deletes the app plus its whole deployment history. Relay
// steps are best-effort; state is deleted last so a failed teardown leaves
// delete retryable.
func (d *Deployer) Delete(ctx context.Context, appName string) error {
	defer d.lockApp(appName)()
	if _, err := d.store.GetApp(appName); err != nil {
		return err
	}
	deps, err := d.store.ListDeployments(appName)
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if dep.Status != "running" {
			continue
		}
		_ = d.runtime.Stop(ctx, dep.ContainerID)
		if dep.PR > 0 {
			if err := d.routes.RemoveRoute(d.hostForPreview(appName, dep.PR)); err != nil {
				return fmt.Errorf("unroute: %w", err)
			}
		}
	}
	if host, ok := d.primaryHost(appName); ok {
		if err := d.routes.RemoveRoute(host); err != nil {
			return fmt.Errorf("unroute: %w", err)
		}
		if d.registrar != nil {
			_ = d.registrar.Deregister(host)
		}
	}
	if err := d.removeCustomDomainRoute(appName); err != nil {
		return err
	}
	return d.store.DeleteApp(appName)
}
