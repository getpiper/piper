package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	archive "github.com/moby/go-archive"
)

// DockerRuntime implements Runtime over the Docker Engine SDK.
type DockerRuntime struct{ cli *docker.Client }

func NewDockerRuntime() (*DockerRuntime, error) {
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &DockerRuntime{cli: cli}, nil
}

func (d *DockerRuntime) Build(ctx context.Context, srcDir, imageTag string) (BuildResult, error) {
	tarball, err := archive.TarWithOptions(srcDir, &archive.TarOptions{})
	if err != nil {
		return BuildResult{}, err
	}
	defer tarball.Close()
	resp, err := d.cli.ImageBuild(ctx, tarball, build.ImageBuildOptions{
		Tags:       []string{imageTag},
		Remove:     true,
		Dockerfile: "Dockerfile",
	})
	if err != nil {
		return BuildResult{}, err
	}
	defer resp.Body.Close()
	// Drain the build log stream; a failed build leaves no tagged image, which
	// surfaces below as an ImageInspect "not found" error.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return BuildResult{}, err
	}
	insp, err := d.cli.ImageInspect(ctx, imageTag)
	if err != nil {
		return BuildResult{}, fmt.Errorf("inspect built image (build may have failed): %w", err)
	}
	return BuildResult{ImageID: insp.ID}, nil
}

func (d *DockerRuntime) Run(ctx context.Context, imageTag string, containerPort int, env map[string]string) (RunResult, error) {
	port := nat.Port(fmt.Sprintf("%d/tcp", containerPort))
	var envv []string
	for k, v := range env {
		envv = append(envv, k+"="+v)
	}
	created, err := d.cli.ContainerCreate(ctx,
		&container.Config{
			Image:        imageTag,
			Env:          envv,
			ExposedPorts: nat.PortSet{port: struct{}{}},
		},
		&container.HostConfig{
			PortBindings: nat.PortMap{port: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "0"}}},
		}, nil, nil, "")
	if err != nil {
		return RunResult{}, err
	}
	result := RunResult{ContainerID: created.ID}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return result, err
	}
	insp, err := d.cli.ContainerInspect(ctx, created.ID)
	if err != nil {
		return result, err
	}
	bindings := insp.NetworkSettings.Ports[port]
	if len(bindings) == 0 {
		return result, fmt.Errorf("no host port bound for %s", port)
	}
	hp, err := nat.ParsePort(bindings[0].HostPort)
	if err != nil {
		return result, err
	}
	result.HostPort = hp
	return result, nil
}

func (d *DockerRuntime) WaitHealthy(ctx context.Context, hostPort int) error {
	deadline := time.Now().Add(30 * time.Second)
	addr := fmt.Sprintf("127.0.0.1:%d", hostPort)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("container did not become healthy on %s within 30s", addr)
}

func (d *DockerRuntime) Stop(ctx context.Context, containerID string) error {
	timeout := 10
	_ = d.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
	return d.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

func (d *DockerRuntime) Logs(ctx context.Context, containerID string) (io.ReadCloser, error) {
	return d.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: "200",
	})
}
