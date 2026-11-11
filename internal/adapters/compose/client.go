// Package compose implements the runtime adapter on top of Docker Compose.
//
// It is the supported runtime and needs only a local Docker daemon with the
// Compose plugin, so it can be developed and tested on a laptop.
package compose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Client is a thin wrapper around the docker compose CLI. The CLI is used
// instead of the Docker SDK because it is what an operator has installed and
// because it keeps the adapter free of a daemon connection.
type Client struct {
	// ProjectDir is the directory holding the generated compose.yaml.
	ProjectDir string
	// ProjectName is the compose project name, unique per session.
	ProjectName string
	// Docker is the docker binary path.
	Docker string
	// Env is the environment of the compose process.
	Env []string

	mu sync.Mutex
}

// ErrDockerUnavailable is returned when the docker binary or the compose
// plugin cannot be used. It is distinct from a failed command so the core can
// report "install Docker" instead of "command failed".
var ErrDockerUnavailable = errors.New("docker compose is unavailable")

// NewClient returns a client for one compose project.
func NewClient(projectDir, projectName string) *Client {
	return &Client{
		ProjectDir:  projectDir,
		ProjectName: projectName,
		Docker:      "docker",
	}
}

// Available reports whether the docker CLI and the compose plugin exist.
func (c *Client) Available(ctx context.Context) error {
	if _, err := exec.LookPath(c.Docker); err != nil {
		return fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	out, err := c.run(ctx, "compose", "version")
	if err != nil {
		return fmt.Errorf("%w: compose plugin not usable: %v", ErrDockerUnavailable, err)
	}
	if !strings.Contains(out.stdout, "Docker Compose") {
		return fmt.Errorf("%w: unexpected compose version output %q", ErrDockerUnavailable, out.stdout)
	}
	return nil
}

// Up brings the project up detached.
//
// There is deliberately no --wait: a task environment is allowed to come up
// degraded, which is the whole point of an incident simulator. Waiting for
// healthy here would fail every task whose fault is visible at boot.
func (c *Client) Up(ctx context.Context) error {
	_, err := c.run(ctx, "compose", "up", "--detach", "--remove-orphans")
	return err
}

// Down stops and removes the project.
func (c *Client) Down(ctx context.Context) error {
	_, err := c.run(ctx, "compose", "down", "--volumes", "--remove-orphans")
	return err
}

// Stop stops the project without removing it.
func (c *Client) Stop(ctx context.Context) error {
	_, err := c.run(ctx, "compose", "stop")
	return err
}

// Start starts an already created project.
func (c *Client) Start(ctx context.Context) error {
	_, err := c.run(ctx, "compose", "start")
	return err
}

// Restart applies a configuration change to one service.
//
// It recreates the container instead of restarting the process: `compose
// restart` keeps the old container definition, so an environment patch would
// be silently ignored and a participant's fix would appear to do nothing.
// There is deliberately no --wait: a service in an incident is unhealthy, and
// waiting for healthy here would block the session on the very fault the
// participant is asked to diagnose.
func (c *Client) Restart(ctx context.Context, service string) error {
	_, err := c.run(ctx, "compose", "up", "--detach", "--no-deps", "--force-recreate", "--no-build", service)
	return err
}

// PSNames returns the running containers of the project.
func (c *Client) PSNames(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "compose", "ps", "--quiet")
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out.stdout), nil
}

// ContainerID returns the id of the container currently backing a service.
// Reading the port from the container id rather than from the service name is
// deliberate: during a recreate `compose port` can still answer with the
// mapping of the container being replaced.
func (c *Client) ContainerID(ctx context.Context, service string) (string, error) {
	out, err := c.run(ctx, "compose", "ps", "--quiet", service)
	if err != nil {
		return "", err
	}
	lines := nonEmptyLines(out.stdout)
	if len(lines) == 0 {
		return "", fmt.Errorf("no running container for service %s", service)
	}
	return lines[0], nil
}

// Port returns the host port a container port is published on.
func (c *Client) Port(ctx context.Context, service string, containerPort int) (string, error) {
	containerID, err := c.ContainerID(ctx, service)
	if err != nil {
		return "", err
	}
	out, err := c.docker(ctx, "port", containerID, fmt.Sprint(containerPort))
	if err != nil {
		return "", err
	}
	lines := nonEmptyLines(out.stdout)
	if len(lines) == 0 {
		return "", fmt.Errorf("no published port for %s:%d", service, containerPort)
	}
	return strings.TrimSpace(lines[0]), nil
}

// Exec runs a command inside a service.
// Exec runs a captured command inside a service: -T disables the pseudo
// terminal, which is what makes the output a plain byte stream the caller can
// read. The interactive variant keeps the terminal, because a shell without
// one is unusable.
func (c *Client) Exec(ctx context.Context, service string, command []string) (commandOutput, error) {
	args := append([]string{"compose", "exec", "-T", service}, command...)
	return c.run(ctx, args...)
}

// ExecTerminal runs a command inside a service with a terminal allocated, for
// an interactive shell over a transport that can carry one.
func (c *Client) ExecTerminal(ctx context.Context, service string, command []string) (commandOutput, error) {
	args := append([]string{"compose", "exec", service}, command...)
	return c.run(ctx, args...)
}

// Logs returns the logs of one service.
func (c *Client) Logs(ctx context.Context, service string, tail int) (commandOutput, error) {
	args := []string{"compose", "logs", "--no-color", "--timestamps"}
	if tail > 0 {
		args = append(args, "--tail", fmt.Sprint(tail))
	}
	args = append(args, service)
	return c.run(ctx, args...)
}

// RunningServices returns the names of the services of the project that are
// currently running. It is how Up detects a service that starts and
// immediately exits, which --wait cannot catch for a service without a
// healthcheck.
func (c *Client) RunningServices(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "compose", "ps", "--services", "--status", "running")
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out.stdout), nil
}

// InspectEnvironment returns the environment of a running service, which is how
// a policy check sees the participant's fix.
func (c *Client) InspectEnvironment(ctx context.Context, service string) (map[string]string, error) {
	out, err := c.run(ctx, "compose", "exec", "-T", service, "env")
	if err != nil {
		return nil, err
	}
	return parseEnv(out.stdout), nil
}

type commandOutput struct {
	stdout string
	stderr string
}

// docker runs a plain docker command with the project context, so the
// container-scoped commands resolve against the same project.
func (c *Client) docker(ctx context.Context, args ...string) (commandOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cmd := exec.CommandContext(ctx, c.Docker, args...)
	cmd.Dir = c.ProjectDir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Env = append(cmd.Env, "COMPOSE_PROJECT_NAME="+c.ProjectName)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return commandOutput{stdout: stdout.String(), stderr: stderr.String()},
			fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, firstLine(stderr.String()))
	}
	return commandOutput{stdout: stdout.String(), stderr: stderr.String()}, nil
}

func (c *Client) run(ctx context.Context, args ...string) (commandOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cmd := exec.CommandContext(ctx, c.Docker, args...)
	cmd.Dir = c.ProjectDir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Env = append(cmd.Env,
		"COMPOSE_PROJECT_NAME="+c.ProjectName,
		"COMPOSE_FILE="+filepath.Join(c.ProjectDir, composeFile),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := commandOutput{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, firstLine(stderr.String()))
	}
	return out, nil
}

// parseEnv turns `env` output into a map.
func parseEnv(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			continue
		}
		out[key] = value
	}
	return out
}

func nonEmptyLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func firstLine(text string) string {
	lines := nonEmptyLines(text)
	if len(lines) == 0 {
		return "no output"
	}
	return lines[0]
}
