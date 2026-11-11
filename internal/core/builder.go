// Package core provides the Builder that turns a participant workspace into a
// deployable artifact for Docker Compose.
//
// Building happens in a throw-away container (no docker.sock on the host is
// needed beyond the docker CLI the compose engine already requires), with the
// network closed after module warmup and with resource limits so a malicious
// or buggy task cannot freeze the developer machine.
package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	stdRuntime "runtime"
	"strings"
	"time"

	"incidentsim/internal/runtime"
)

// DefaultBuildTimeout bounds a single go build run in a container.
const DefaultBuildTimeout = 60 * time.Second

// GoBuilder builds a code-level workspace in a one-shot container and returns
// an Artifact of Kind "binary".
type GoBuilder struct {
	// Image is the container image used to build (golang:1.24-alpine by
	// default). Overridable in tests.
	Image string
	// Timeout bounds the build. Defaults to DefaultBuildTimeout.
	Timeout time.Duration
	// Memory, CPUs and Pids limit the build container.
	Memory string
	CPUs   string
	Pids   string
	// BuildCache is the named docker volume used for the Go module cache,
	// so repeated builds are fast.
	BuildCache string
}

// Build compiles workspaceDir into a static Linux binary. On failure it
// returns the compiler output so the participant can see it, and the running
// environment is never touched.
func (b *GoBuilder) Build(ctx context.Context, workspaceDir string) (*runtime.Artifact, error) {
	if b.Image == "" {
		b.Image = "golang:1.24-alpine"
	}
	if b.Timeout <= 0 {
		b.Timeout = DefaultBuildTimeout
	}
	if b.BuildCache == "" {
		b.BuildCache = "incidentsim-go-build-cache"
	}

	digest, err := hashDirectory(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("hash workspace: %w", err)
	}

	// Output directory mounted into the container so the built binary survives
	// the throw-away container's removal.
	outDir, err := os.MkdirTemp("", "incidentsim-build-*")
	if err != nil {
		return nil, fmt.Errorf("create build output dir: %w", err)
	}
	completed := false
	defer func() {
		if !completed {
			_ = os.RemoveAll(outDir)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, b.Timeout)
	defer cancel()

	// Warm the module cache first with network, then rebuild with the network
	// closed. A cold cache needs to download modules; a warm one must not
	// reach the network.
	baseArgs := b.containerArgs()
	target, err := buildTarget(workspaceDir)
	if err != nil {
		return nil, err
	}
	steps := [][]string{
		append(append([]string{"run", "--rm"}, baseArgs...), []string{"-w", "/src", "-v", workspaceDir + ":/src:ro", b.Image, "go", "mod", "download"}...),
		append(append([]string{"run", "--rm", "--network", "none"}, baseArgs...), []string{"-v", outDir + ":/out", "-w", "/src", "-v", workspaceDir + ":/src:ro", b.Image, "go", "build", "-trimpath", "-o", "/out/app", target}...),
	}
	for _, step := range steps {
		cmd := exec.CommandContext(ctx, "docker", step...)
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("docker build step %q failed: %w\n%s", strings.Join(step, " "), err, stderr.String())
		}
		_ = out
	}

	binaryPath := filepath.Join(outDir, "app")
	completed = true
	return &runtime.Artifact{
		Service: "app",
		Kind:    "binary",
		Path:    binaryPath,
		Digest:  digest,
	}, nil
}

// buildTarget follows the scenario workspace contract: applications normally
// live in workspace/app, while small pure-code tasks may keep package main at
// the workspace root.
func buildTarget(workspaceDir string) (string, error) {
	appDir := filepath.Join(workspaceDir, "app")
	entries, err := os.ReadDir(appDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
				return "./app", nil
			}
		}
	}
	entries, err = os.ReadDir(workspaceDir)
	if err != nil {
		return "", fmt.Errorf("read workspace: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			return ".", nil
		}
	}
	return "", fmt.Errorf("no Go package in workspace %s", workspaceDir)
}

// containerArgs returns the common constraints for one-shot build containers.
// The Go variables are docker -e settings because environment variables of the
// docker client do not configure the process in the container.
func (b *GoBuilder) containerArgs() []string {
	return []string{
		"--memory", valueOrDefault(b.Memory, "512m"),
		"--cpus", valueOrDefault(b.CPUs, "1"),
		"--pids-limit", valueOrDefault(b.Pids, "128"),
		"-e", "GOOS=linux",
		"-e", "GOARCH=" + stdRuntime.GOARCH,
		"-e", "CGO_ENABLED=0",
		"-v", b.BuildCache + ":/go/pkg/mod",
	}
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func hashDirectory(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
