// Package contract contains the runtime.Adapter contract suite. Run it against
// any runtime.Adapter implementation (fake, compose, firecracker) to prove it
// honors the runtime contract:
//
//   - Up is idempotent per session id
//   - Exec, Logs and Metrics work against a running environment
//   - Deploy installs binary and files artifacts
//   - Reset restores the baseline and keeps the session valid
//   - Down is safe for an unknown session
//
// Usage:
//
//	contract.Run(t, func(t *testing.T) (runtime.Adapter, func()) {
//		return runtime.NewFake("compose"), func() {}
//	}, "fake-compose")
package contract

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"incidentsim/internal/runtime"
)

// Run executes the contract suite against factory. Every subtest receives a
// fresh adapter from factory; the returned cleanup runs when the subtest ends.
// name identifies the engine in failure messages, e.g. "fake-compose" or
// "compose".
func Run(t *testing.T, factory func(t *testing.T) (runtime.Adapter, func()), name string) {
	t.Helper()

	const (
		sessionID = "contract-session"
		unknownID = "contract-unknown-session"
		upTimeout = 3 * time.Minute
	)

	newCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), upTimeout)
	}

	t.Run("UpIsIdempotent", func(t *testing.T) {
		adapter, cleanup := factory(t)
		t.Cleanup(cleanup)
		ctx, cancel := newCtx()
		defer cancel()

		first, err := adapter.Up(ctx, sessionID, contractSpec())
		if err != nil {
			t.Fatalf("Up: %v", err)
		}
		second, err := adapter.Up(ctx, sessionID, contractSpec())
		if err != nil {
			t.Fatalf("second Up: %v", err)
		}
		if second == nil {
			t.Fatal("second Up returned a nil instance")
		}
		if second.ID != first.ID || second.Engine != first.Engine {
			t.Fatalf("second Up returned a different instance:\n got: %+v\nwant: %+v", second, first)
		}
		if len(second.Services) != len(first.Services) {
			t.Fatalf("service count changed between Ups: %d vs %d", len(second.Services), len(first.Services))
		}
		for i := range first.Services {
			if second.Services[i].Name != first.Services[i].Name {
				t.Fatalf("service %d changed between Ups: %q vs %q",
					i, second.Services[i].Name, first.Services[i].Name)
			}
		}
	})

	t.Run("ExecLogsMetricsWork", func(t *testing.T) {
		adapter, cleanup := factory(t)
		t.Cleanup(cleanup)
		ctx, cancel := newCtx()
		defer cancel()

		if _, err := adapter.Up(ctx, sessionID, contractSpec()); err != nil {
			t.Fatalf("Up: %v", err)
		}

		result, err := adapter.Exec(ctx, sessionID, runtime.ExecRequest{
			Service: "app",
			Command: []string{"true"},
			Timeout: runtime.DefaultExecTimeout,
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if result.ExitCode != 0 {
			t.Fatalf("Exec exit code = %d, stdout = %q, stderr = %q, want 0",
				result.ExitCode, result.Stdout, result.Stderr)
		}

		if _, err := adapter.Logs(ctx, sessionID, runtime.LogOptions{Service: "app"}); err != nil {
			t.Fatalf("Logs: %v", err)
		}

		samples, err := adapter.Metrics(ctx, sessionID, "app")
		if err != nil {
			t.Fatalf("Metrics: %v", err)
		}
		if len(samples) == 0 {
			t.Fatal("Metrics returned no samples")
		}
		for _, sample := range samples {
			if sample.Name == "" {
				t.Errorf("metric sample without a name: %+v", sample)
			}
		}
	})

	t.Run("DeployBinaryArtifact", func(t *testing.T) {
		adapter, cleanup := factory(t)
		t.Cleanup(cleanup)
		ctx, cancel := newCtx()
		defer cancel()

		if _, err := adapter.Up(ctx, sessionID, contractSpec()); err != nil {
			t.Fatalf("Up: %v", err)
		}

		bin := filepath.Join(t.TempDir(), "service")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho contract-binary\n"), 0o755); err != nil {
			t.Fatalf("write test binary: %v", err)
		}

		artifact := runtime.Artifact{
			Service: "app",
			Kind:    "binary",
			Path:    bin,
			Digest:  "sha256:contract-binary",
		}
		instance, err := adapter.Deploy(ctx, sessionID, artifact, true)
		if err != nil {
			t.Fatalf("Deploy binary: %v", err)
		}
		if instance == nil || instance.ID != sessionID {
			t.Fatalf("Deploy binary returned %+v, want an instance of session %q", instance, sessionID)
		}
	})

	t.Run("DeployFilesArtifact", func(t *testing.T) {
		adapter, cleanup := factory(t)
		t.Cleanup(cleanup)
		ctx, cancel := newCtx()
		defer cancel()

		if _, err := adapter.Up(ctx, sessionID, contractSpec()); err != nil {
			t.Fatalf("Up: %v", err)
		}

		artifact := runtime.Artifact{
			Service: "app",
			Kind:    "files",
			Files: map[string][]byte{
				"/etc/app/app.conf": []byte("LOG_LEVEL=debug\n"),
			},
			Digest: "sha256:contract-files",
		}
		instance, err := adapter.Deploy(ctx, sessionID, artifact, false)
		if err != nil {
			t.Fatalf("Deploy files: %v", err)
		}
		if instance == nil || instance.ID != sessionID {
			t.Fatalf("Deploy files returned %+v, want an instance of session %q", instance, sessionID)
		}
	})

	t.Run("ResetRestoresBaseline", func(t *testing.T) {
		adapter, cleanup := factory(t)
		t.Cleanup(cleanup)
		ctx, cancel := newCtx()
		defer cancel()

		before, err := adapter.Up(ctx, sessionID, contractSpec())
		if err != nil {
			t.Fatalf("Up: %v", err)
		}

		reset, err := adapter.Reset(ctx, sessionID)
		if err != nil {
			t.Fatalf("Reset: %v", err)
		}
		if reset == nil {
			t.Fatal("Reset returned a nil instance")
		}
		if reset.ID != sessionID {
			t.Fatalf("Reset instance ID = %q, want %q", reset.ID, sessionID)
		}
		if len(reset.Services) == 0 {
			t.Fatal("Reset returned no services")
		}
		if len(reset.Services) != len(before.Services) {
			t.Fatalf("Reset changed the service count: %d vs %d", len(reset.Services), len(before.Services))
		}
		for i := range before.Services {
			if reset.Services[i].Name != before.Services[i].Name {
				t.Fatalf("Reset changed service %d: %q vs %q",
					i, reset.Services[i].Name, before.Services[i].Name)
			}
		}

		// The session must remain usable after a reset.
		if _, err := adapter.Exec(ctx, sessionID, runtime.ExecRequest{
			Service: "app",
			Command: []string{"true"},
			Timeout: runtime.DefaultExecTimeout,
		}); err != nil {
			t.Fatalf("Exec after reset: %v", err)
		}
	})

	t.Run("DownSafeForUnknownSession", func(t *testing.T) {
		adapter, _ := factory(t)
		ctx, cancel := newCtx()
		defer cancel()

		if err := adapter.Down(ctx, unknownID); err != nil {
			t.Fatalf("Down of unknown session: %v", err)
		}
	})
}

// contractSpec is the minimal manifest every contract subtest boots with. The
// fake ignores it; compose uses it to render its project.
func contractSpec() runtime.Spec {
	return runtime.Spec{
		Snapshot: "contract-baseline",
		Services: []runtime.ServiceSpec{
			{
				Name:  "app",
				Image: "incidentsim/workload:dev",
				Ports: []int{8080},
				Healthcheck: &runtime.HealthcheckSpec{
					Path:            "/healthz",
					Port:            8080,
					IntervalSeconds: 1,
					TimeoutSeconds:  1,
				},
			},
		},
	}
}
