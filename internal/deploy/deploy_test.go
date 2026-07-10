package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/runtime"
	"github.com/getpiper/piper/internal/store"
)

type fakeCaddy struct {
	upserts   map[string]int
	removes   []string
	tlsRoutes []string
	removeErr error
}

func newFakeCaddy() *fakeCaddy {
	return &fakeCaddy{upserts: make(map[string]int)}
}

func (f *fakeCaddy) UpsertRoute(host string, port int) error {
	f.upserts[host] = port
	return nil
}

func (f *fakeCaddy) UpsertRouteTLS(host string, port int) error {
	f.tlsRoutes = append(f.tlsRoutes, fmt.Sprintf("%s->%d", host, port))
	return nil
}

func (f *fakeCaddy) RemoveRoute(host string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removes = append(f.removes, host)
	return nil
}

func (f *fakeCaddy) removed() []string { return f.removes }

func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploy.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return s, path
}

func deploymentCountWithStatus(t *testing.T, path, status string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("Open database: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM deployments WHERE status=?`, status).Scan(&count); err != nil {
		t.Fatalf("Count deployments: %v", err)
	}
	return count
}

func TestDeploySuccessRoutesAndRecords(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	dep, err := d.Deploy(context.Background(), "blog", t.TempDir())
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if dep.Status != "running" {
		t.Errorf("status = %q, want running", dep.Status)
	}
	if routes.upserts["blog.piper.localhost"] != 40001 {
		t.Errorf("routes = %+v, want blog.piper.localhost -> 40001", routes.upserts)
	}
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ContainerID != "c1" {
		t.Errorf("container ID = %q, want c1", got.ContainerID)
	}
}

func TestDeploySecondStopsPrevious(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	rt.BuildResultVal = runtime.BuildResult{ImageID: "img2"}
	rt.RunResultVal = runtime.RunResult{ContainerID: "c2", HostPort: 40002}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}

	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1]", rt.Stopped)
	}
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ContainerID != "c2" {
		t.Errorf("container ID = %q, want c2", got.ContainerID)
	}
}

func TestDeployHealthFailureStopsContainerAndRecordsFailed(t *testing.T) {
	s, path := newStore(t)
	healthErr := errors.New("unhealthy")
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
		HealthErr:      healthErr,
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); !errors.Is(err, healthErr) {
		t.Fatalf("Deploy error = %v, want health error", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1]", rt.Stopped)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Errorf("failed deployment count = %d, want 1", got)
	}
	if _, err := s.LatestRunning("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestRunning error = %v, want ErrNotFound", err)
	}
}

func TestDeployBuildFailureRecordsFailed(t *testing.T) {
	s, path := newStore(t)
	buildErr := errors.New("build failed")
	rt := &runtime.FakeRuntime{BuildErr: buildErr}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); !errors.Is(err, buildErr) {
		t.Fatalf("Deploy error = %v, want build error", err)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Errorf("failed deployment count = %d, want 1", got)
	}
}

func TestDeployRunFailureRecordsFailed(t *testing.T) {
	s, path := newStore(t)
	runErr := errors.New("run failed")
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunErr:         runErr,
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); !errors.Is(err, runErr) {
		t.Fatalf("Deploy error = %v, want run error", err)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Errorf("failed deployment count = %d, want 1", got)
	}
}

func TestDeployCancelledRunCleansPartialContainerWithLiveContext(t *testing.T) {
	s, path := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "partial-c"},
		RunErr:         context.Canceled,
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(ctx, "blog", t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Deploy error = %v, want context.Canceled", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "partial-c" {
		t.Fatalf("stopped = %v, want [partial-c]", rt.Stopped)
	}
	if len(rt.StopContextErrs) != 1 || rt.StopContextErrs[0] != nil {
		t.Fatalf("stop context errors = %v, want [nil]", rt.StopContextErrs)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Fatalf("failed deployment count = %d, want 1", got)
	}
}

func TestDeployPreviewRoutesFlattenedHostAndKeepsMain(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "main-c", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	rt.RunResultVal = runtime.RunResult{ContainerID: "preview-c", HostPort: 40002}
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("DeployPreview: %v", err)
	}

	if routes.upserts["pr-5-blog.piper.localhost"] != 40002 {
		t.Errorf("routes = %+v, want pr-5-blog.piper.localhost -> 40002", routes.upserts)
	}
	if len(rt.Stopped) != 0 {
		t.Errorf("stopped = %v, want none (main must survive)", rt.Stopped)
	}
	main, err := s.LatestRunning("blog")
	if err != nil || main.ContainerID != "main-c" {
		t.Errorf("main running = %+v (err %v), want main-c", main, err)
	}
}

func TestDeployPreviewSecondStopsPreviousPreview(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "p1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("first DeployPreview: %v", err)
	}
	rt.RunResultVal = runtime.RunResult{ContainerID: "p2", HostPort: 40002}
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("second DeployPreview: %v", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "p1" {
		t.Errorf("stopped = %v, want [p1]", rt.Stopped)
	}
}

func TestTeardownPreviewStopsAndUnroutes(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "p1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("DeployPreview: %v", err)
	}

	if err := d.TeardownPreview(context.Background(), "blog", 5); err != nil {
		t.Fatalf("TeardownPreview: %v", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "p1" {
		t.Errorf("stopped = %v, want [p1]", rt.Stopped)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != "pr-5-blog.piper.localhost" {
		t.Errorf("removed = %v, want [pr-5-blog.piper.localhost]", routes.removed())
	}
	if _, err := s.PreviewRunning("blog", 5); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PreviewRunning after teardown err = %v, want ErrNotFound", err)
	}
}

func TestTeardownPreviewNoRunningIsNoOp(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if err := d.TeardownPreview(context.Background(), "blog", 99); err != nil {
		t.Fatalf("TeardownPreview no-op err = %v, want nil", err)
	}
}

type fakeRegistrar struct {
	host    string
	deregs  []string
	failing bool
}

func (f *fakeRegistrar) Register(app string) (string, error) {
	if f.failing {
		return "", errors.New("quota")
	}
	f.host = "hash-" + app + "-alice.public.getpiper.co"
	return f.host, nil
}
func (f *fakeRegistrar) Deregister(hostname string) error {
	f.deregs = append(f.deregs, hostname)
	return nil
}

func TestDeployTerminatedRoutesAssignedHostname(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// Route must be the relay-assigned single-label host, NOT blog.public.getpiper.co.
	if _, ok := routes.upserts["hash-blog-alice.public.getpiper.co"]; !ok {
		t.Fatalf("routes = %v, want the assigned hostname", routes.upserts)
	}
	if _, ok := routes.upserts["blog.public.getpiper.co"]; ok {
		t.Fatal("terminated deploy must not route <app>.<baseDom>")
	}
}

func TestDeployTerminatedRegistrarFails(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "public.getpiper.co")
	d.SetHostnameRegistrar(&fakeRegistrar{failing: true})
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected deploy to fail when registration fails")
	}
}

func deploymentLog(t *testing.T, s *store.Store, app string) string {
	t.Helper()
	deps, err := s.ListDeployments(app)
	if err != nil || len(deps) == 0 {
		t.Fatalf("ListDeployments: %v (%d rows)", err, len(deps))
	}
	logs, err := s.DeploymentLogs(app, deps[0].ID)
	if err != nil {
		t.Fatalf("DeploymentLogs: %v", err)
	}
	return logs
}

func TestDeployBuildFailurePersistsBuildLog(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{Log: "Step 1/2 : FROM busybox\nboom\n"},
		BuildErr:       errors.New("build failed"),
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected build error")
	}
	logs := deploymentLog(t, s, "blog")
	if !strings.Contains(logs, "boom") {
		t.Errorf("failed deployment logs = %q, want build output", logs)
	}
}

func TestDeployHealthFailureAppendsContainerOutput(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1", Log: "build ok\n"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
		HealthErr:      errors.New("unhealthy"),
		LogsVal:        "panic: kaboom\n",
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected health error")
	}
	logs := deploymentLog(t, s, "blog")
	for _, want := range []string{"build ok", "--- container output ---", "panic: kaboom", "unhealthy"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
	if strings.Index(logs, "build ok") > strings.Index(logs, "container output") {
		t.Error("build log must precede container output")
	}
	if strings.Index(logs, "panic: kaboom") > strings.Index(logs, "unhealthy") {
		t.Error("error text must follow container output")
	}
}

func TestDeployBuildFailureFromStageRecordsErrorInLog(t *testing.T) {
	s, _ := newStore(t)
	buildErr := errors.New(`The command '/bin/sh -c false' returned a non-zero code: 7`)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{Log: ""},
		BuildErr:       buildErr,
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected build error")
	}
	logs := deploymentLog(t, s, "blog")
	if !strings.Contains(logs, buildErr.Error()) {
		t.Errorf("failed deployment logs = %q, want build error text", logs)
	}
}

func TestDeploySuccessPersistsBuildLog(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1", Log: "build ok\n"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	logs := deploymentLog(t, s, "blog")
	if logs != "build ok\n" {
		t.Errorf("logs = %q, want build log only (no container output on success)", logs)
	}
}

func TestDeployRoutesCustomDomainWhenActive(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	if err := s.SetDomainConfig("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDomainStatus("shop.dev", "active", "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	want := "blog.shop.dev->40001"
	found := false
	for _, r := range routes.tlsRoutes {
		found = found || r == want
	}
	if !found {
		t.Fatalf("tlsRoutes = %v, want %s", routes.tlsRoutes, want)
	}
}

func TestDeploySkipsCustomDomainWhenNotActive(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	// Fresh config is "issuing": no cert is armed yet, so no TLS route.
	if err := s.SetDomainConfig("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(routes.tlsRoutes) != 0 {
		t.Fatalf("tlsRoutes = %v, want none while domain is issuing", routes.tlsRoutes)
	}

	// A failed config must not get one either.
	if err := s.UpdateDomainStatus("shop.dev", "failed", "acme: boom", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(routes.tlsRoutes) != 0 {
		t.Fatalf("tlsRoutes = %v, want none while domain is failed", routes.tlsRoutes)
	}
}

func TestDeploySkipsCustomDomainWhenAbsent(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(routes.tlsRoutes) != 0 {
		t.Fatalf("tlsRoutes = %v, want none without an active custom domain", routes.tlsRoutes)
	}
}

func TestDeployCombinedLogIsTailCapped(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1", Log: strings.Repeat("b", runtime.LogCap)},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
		HealthErr:      errors.New("unhealthy"),
		LogsVal:        "THE END",
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err == nil {
		t.Fatal("expected health error")
	}
	logs := deploymentLog(t, s, "blog")
	if !strings.HasPrefix(logs, "[log truncated]\n") {
		t.Error("combined log over cap must carry the truncation marker")
	}
	if !strings.Contains(logs, "THE END") {
		t.Error("tail (container output) must be kept")
	}
	if !strings.HasSuffix(logs, "unhealthy\n") {
		t.Error("error text must be the tail (recorded after container output)")
	}
}

func TestStopRetiresRunningAndRemovesRoute(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1]", rt.Stopped)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != "blog.piper.localhost" {
		t.Errorf("removed = %v, want [blog.piper.localhost]", routes.removed())
	}
	if _, err := s.LatestRunning("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestRunning after stop err = %v, want ErrNotFound", err)
	}
	// App and history remain: latest deployment is the same row, now stopped.
	dep, err := s.LatestDeployment("blog")
	if err != nil || dep.Status != "stopped" || dep.ContainerID != "c1" {
		t.Errorf("latest = %+v (err %v), want c1 stopped", dep, err)
	}
}

func TestStopNothingRunningIsNoOp(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop no-op err = %v, want nil", err)
	}
	if len(rt.Stopped) != 0 || len(routes.removed()) != 0 {
		t.Errorf("no-op stop touched runtime/routes: %v %v", rt.Stopped, routes.removed())
	}
}

func TestStopUnknownAppIsNotFound(t *testing.T) {
	s, _ := newStore(t)
	d := New(s, &runtime.FakeRuntime{}, newFakeCaddy(), "piper.localhost")
	if err := d.Stop(context.Background(), "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Stop(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestStopRemovesCustomDomainRoute(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if err := s.SetDomainConfig("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDomainStatus("shop.dev", "active", "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got := routes.removed()
	want := map[string]bool{"blog.piper.localhost": true, "blog.shop.dev": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Errorf("removed = %v, want both primary and custom-domain hosts", got)
	}
}

func TestStopTerminatedRemovesAssignedHostname(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != "hash-blog-alice.public.getpiper.co" {
		t.Errorf("removed = %v, want the assigned hostname", routes.removed())
	}
	if len(reg.deregs) != 0 {
		t.Errorf("deregs = %v, stop must not deregister (delete-only)", reg.deregs)
	}
}

func TestStopTerminatedRelayDownSkipsRouteBestEffort(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	reg.failing = true // relay unreachable at stop time
	if err := d.Stop(context.Background(), "blog"); err != nil {
		t.Fatalf("Stop with relay down err = %v, want nil (best-effort)", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1] despite relay outage", rt.Stopped)
	}
	if len(routes.removed()) != 0 {
		t.Errorf("removed = %v, want none (hostname unknown)", routes.removed())
	}
	dep, err := s.LatestDeployment("blog")
	if err != nil || dep.Status != "stopped" {
		t.Errorf("latest = %+v (err %v), want stopped", dep, err)
	}
}

func TestDeleteTearsDownProductionAndPreviews(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "main-c", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	rt.RunResultVal = runtime.RunResult{ContainerID: "preview-c", HostPort: 40002}
	if _, err := d.DeployPreview(context.Background(), "blog", 5, t.TempDir()); err != nil {
		t.Fatalf("DeployPreview: %v", err)
	}

	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	stopped := map[string]bool{}
	for _, id := range rt.Stopped {
		stopped[id] = true
	}
	if !stopped["main-c"] || !stopped["preview-c"] {
		t.Errorf("stopped = %v, want main-c and preview-c", rt.Stopped)
	}
	removed := map[string]bool{}
	for _, h := range routes.removed() {
		removed[h] = true
	}
	if !removed["blog.piper.localhost"] || !removed["pr-5-blog.piper.localhost"] {
		t.Errorf("removed = %v, want production and preview hosts", routes.removed())
	}
	if _, err := s.GetApp("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetApp after delete err = %v, want ErrNotFound", err)
	}
	if deps, _ := s.ListDeployments("blog"); len(deps) != 0 {
		t.Errorf("deployments after delete = %v, want none", deps)
	}
}

func TestDeleteUnknownAppIsNotFound(t *testing.T) {
	s, _ := newStore(t)
	d := New(s, &runtime.FakeRuntime{}, newFakeCaddy(), "piper.localhost")
	if err := d.Delete(context.Background(), "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Delete(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestDeleteNothingRunningStillDeletesState(t *testing.T) {
	s, _ := newStore(t) // "blog" exists, never deployed
	rt := &runtime.FakeRuntime{}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(rt.Stopped) != 0 {
		t.Errorf("stopped = %v, want none", rt.Stopped)
	}
	if _, err := s.GetApp("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetApp after delete err = %v, want ErrNotFound", err)
	}
}

func TestDeleteTerminatedDeregistersHostname(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	want := "hash-blog-alice.public.getpiper.co"
	if len(reg.deregs) != 1 || reg.deregs[0] != want {
		t.Errorf("deregs = %v, want [%s]", reg.deregs, want)
	}
	if len(routes.removed()) != 1 || routes.removed()[0] != want {
		t.Errorf("removed = %v, want [%s]", routes.removed(), want)
	}
}

func TestDeleteTerminatedRelayDownStillDeletesState(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "public.getpiper.co")
	reg := &fakeRegistrar{}
	d.SetHostnameRegistrar(reg)
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	reg.failing = true // relay unreachable at delete time
	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete with relay down err = %v, want nil (best-effort)", err)
	}
	if len(reg.deregs) != 0 {
		t.Errorf("deregs = %v, want none (hostname unknown)", reg.deregs)
	}
	if _, err := s.GetApp("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetApp after delete err = %v, want ErrNotFound", err)
	}
}

func TestDeleteRemovesCustomDomainRoute(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if err := s.SetDomainConfig("shop.dev", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDomainStatus("shop.dev", "active", "", time.Now().Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := d.Delete(context.Background(), "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	found := false
	for _, h := range routes.removed() {
		found = found || h == "blog.shop.dev"
	}
	if !found {
		t.Errorf("removed = %v, want blog.shop.dev among them", routes.removed())
	}
}

func TestDeleteRouteRemovalFailureLeavesStateIntact(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	routes.removeErr = errors.New("caddy down")
	if err := d.Delete(context.Background(), "blog"); err == nil {
		t.Fatal("Delete with failing unroute must error")
	}
	if _, err := s.GetApp("blog"); err != nil {
		t.Errorf("GetApp = %v, want app still present (delete stays retryable)", err)
	}
}
