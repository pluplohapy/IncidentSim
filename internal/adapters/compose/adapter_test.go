package compose

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"incidentsim/internal/runtime"
)

// dockerAvailable reports whether the integration tests can run. They are
// skipped, not ignored: a developer without Docker still runs the unit tests.
func dockerAvailable(t *testing.T) bool {
	t.Helper()
	if os.Getenv("INCIDENTSIM_TEST_DOCKER") != "1" {
		t.Skip("set INCIDENTSIM_TEST_DOCKER=1 to run compose integration tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not installed: %v", err)
	}
	client := NewClient(t.TempDir(), "incidentsim-probe")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Available(ctx); err != nil {
		t.Skipf("docker compose not usable: %v", err)
	}
	return true
}

func TestUpFailsLoudlyWithoutDocker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err == nil {
		t.Skip("docker is installed on this machine")
	}
	adapter := New(t.TempDir())
	adapter.Docker = filepath.Join(t.TempDir(), "docker-does-not-exist")
	_, err := adapter.Up(context.Background(), "session-1", testSpec())
	if !errors.Is(err, ErrDockerUnavailable) {
		t.Fatalf("Up error = %v, want ErrDockerUnavailable", err)
	}
}

func TestAdapterReportsUnknownSession(t *testing.T) {
	adapter := New(t.TempDir())
	ctx := context.Background()

	if _, err := adapter.Exec(ctx, "ghost", runtime.ExecRequest{Service: "app"}); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Exec error = %v", err)
	}
	if _, err := adapter.Logs(ctx, "ghost", runtime.LogOptions{Service: "app"}); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Logs error = %v", err)
	}
	if _, err := adapter.Metrics(ctx, "ghost", "app"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Metrics error = %v", err)
	}
	if err := adapter.Reset(ctx, "ghost"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Reset error = %v", err)
	}
	// Down of an unknown session is a no-op, not an error: reset and teardown
	// must be safe to call twice.
	if err := adapter.Down(ctx, "ghost"); err != nil {
		t.Fatalf("Down of unknown session: %v", err)
	}
}

func TestApplyToUnknownServiceIsRejected(t *testing.T) {
	adapter := New(t.TempDir())
	ctx := context.Background()
	_, err := adapter.Up(ctx, "session-1", testSpec())
	if err != nil {
		t.Skipf("cannot bring the project up here: %v", err)
	}
	defer adapter.Down(ctx, "session-1")

	err = adapter.Apply(ctx, "session-1", runtime.EnvPatch{Service: "cache", Vars: map[string]string{"X": "1"}})
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Apply to unknown service = %v, want ErrNotFound", err)
	}
}

func TestApplyRewritesComposeFileWithFix(t *testing.T) {
	adapter := New(t.TempDir())
	ctx := context.Background()
	if _, err := adapter.Up(ctx, "session-1", testSpec()); err != nil {
		t.Skipf("cannot bring the project up here: %v", err)
	}
	defer adapter.Down(ctx, "session-1")

	if err := adapter.Apply(ctx, "session-1", runtime.EnvPatch{
		Service: "app",
		Vars:    map[string]string{"LOG_LEVEL": "info"},
		Restart: true,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	project := filepath.Join(adapter.projectDir("session-1"), composeFile)
	document, err := os.ReadFile(project)
	if err != nil {
		t.Fatalf("read compose file: %v", err)
	}
	if !strings.Contains(string(document), "LOG_LEVEL") {
		t.Fatalf("compose file lost the patched env:\n%s", document)
	}
}

func TestComposeLifecycleAgainstRealDocker(t *testing.T) {
	if !dockerAvailable(t) {
		return
	}
	// The task services come from images that must exist locally. Build them
	// with `make workload-image` before enabling this test.
	t.Skip("requires local images incidentsim/workload:dev and postgres:16-alpine")
}

func TestScrapeMetricsReadsPrometheusText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.Write([]byte("# HELP app_errors_total errors\napp_errors_total 3\napp_disk_used_pct{service=\"app\"} 97.5\n"))
	}))
	defer server.Close()

	ctx := context.Background()
	samples, err := scrapeMetrics(ctx, server.URL+"/metrics", "app")
	if err != nil {
		t.Fatalf("scrapeMetrics: %v", err)
	}
	byName := map[string]float64{}
	for _, sample := range samples {
		byName[sample.Name] = sample.Value
	}
	if byName["app_errors_total"] != 3 {
		t.Errorf("app_errors_total = %v", byName["app_errors_total"])
	}
	if byName["app_disk_used_pct"] != 97.5 {
		t.Errorf("app_disk_used_pct = %v", byName["app_disk_used_pct"])
	}

	if _, err := scrapeMetrics(ctx, server.URL+"/nope", "app"); err == nil {
		t.Error("scraping a non-metrics path succeeded")
	}
}
