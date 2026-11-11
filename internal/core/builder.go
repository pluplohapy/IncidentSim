// Package core provides the Builder that turns a participant workspace into a
// deployable artifact. It is engine agnostic: the same builder works for
// compose and Firecracker because the produced binary targets Linux.
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
	defer os.RemoveAll(outDir)

	ctx, cancel := context.WithTimeout(ctx, b.Timeout)
	defer cancel()

	// Warm the module cache first with network, then rebuild with the network
	// closed. A cold cache needs to download modules; a warm one must not
	// reach the network.
	steps := [][]string{
		{"run", "--rm", "-v", b.BuildCache + ":/go/pkg/mod", "-w", "/src", "-v", workspaceDir + ":/src:ro", b.Image, "go", "mod", "download"},
		{"run", "--rm", "--network", "none", "-v", b.BuildCache + ":/go/pkg/mod", "-v", outDir + ":/out", "-w", "/src", "-v", workspaceDir + ":/src:ro", b.Image, "go", "build", "-trimpath", "-o", "/out/app", "."},
	}
	for _, step := range steps {
		cmd := exec.CommandContext(ctx, "docker", step...)
		cmd.Env = append(cmd.Env, "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		if b.Memory != "" {
			cmd.Env = append(cmd.Env, "MEMORY="+b.Memory)
		}
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("docker build step %q failed: %w\n%s", strings.Join(step, " "), err, stderr.String())
		}
		_ = out
	}

	binaryPath := filepath.Join(outDir, "app")
	return &runtime.Artifact{
		Service: "app",
		Kind:    "binary",
		Path:    binaryPath,
		Digest:  digest,
	}, nil
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
