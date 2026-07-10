package deploy

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/runtime"
	"github.com/getpiper/piper/internal/store"
)

type fakeCaddy struct {
	upserts map[string]int
	removes []string
}

func newFakeCaddy() *fakeCaddy {
	return &fakeCaddy{upserts: make(map[string]int)}
}

func (f *fakeCaddy) UpsertRoute(host string, port int) error {
	f.upserts[host] = port
	return nil
}

func (f *fakeCaddy) RemoveRoute(host string) error {
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
	for _, want := range []string{"build ok", "--- container output ---", "panic: kaboom"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
	if strings.Index(logs, "build ok") > strings.Index(logs, "container output") {
		t.Error("build log must precede container output")
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
	if !strings.HasSuffix(logs, "THE END") {
		t.Error("tail (container output) must be kept")
	}
}
