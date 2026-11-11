package compose

import (
	"context"
	"errors"
	"net"
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
	if os.Getenv("INCIDENTSIM_TEST_DOCKER") == "1" {
		t.Skip("docker integration is enabled in this run")
	}
	adapter := New(t.TempDir())
	adapter.Docker = filepath.Join(t.TempDir(), "docker-does-not-exist")
	_, err := adapter.Up(context.Background(), "session-1", testSpec())
	if !errors.Is(err, ErrDockerUnavailable) {
		t.Fatalf("Up error = %v, want ErrDockerUnavailable", err)
	}
}

func TestAdapterReportsUnknownSession(t *testing.T) {
	if os.Getenv("INCIDENTSIM_TEST_DOCKER") != "1" {
		// The fake-free parts of the adapter are covered by the render tests;
		// the real engine behaviour is an integration concern.
		t.Skip("set INCIDENTSIM_TEST_DOCKER=1 to talk to a real docker daemon")
	}
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
	if _, err := adapter.Reset(ctx, "ghost"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Reset error = %v", err)
	}
	// Down of an unknown session is a no-op, not an error: reset and teardown
	// must be safe to call twice.
	if err := adapter.Down(ctx, "ghost"); err != nil {
		t.Fatalf("Down of unknown session: %v", err)
	}
}

func TestApplyToUnknownServiceIsRejected(t *testing.T) {
	if !dockerAvailable(t) {
		return
	}
	adapter := New(t.TempDir())
	ctx := context.Background()
	_, err := adapter.Up(ctx, "session-1", testSpec())
	if err != nil {
		t.Skipf("cannot bring the project up here: %v", err)
	}
	defer adapter.Down(ctx, "session-1")

	_, err = adapter.Apply(ctx, "session-1", runtime.EnvPatch{Service: "cache", Vars: map[string]string{"X": "1"}})
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Apply to unknown service = %v, want ErrNotFound", err)
	}
}

func TestApplyRewritesComposeFileWithFix(t *testing.T) {
	if !dockerAvailable(t) {
		return
	}
	adapter := New(t.TempDir())
	ctx := context.Background()
	if _, err := adapter.Up(ctx, "session-1", testSpec()); err != nil {
		t.Skipf("cannot bring the project up here: %v", err)
	}
	defer adapter.Down(ctx, "session-1")

	if _, err := adapter.Apply(ctx, "session-1", runtime.EnvPatch{
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

// TestComposeFullLifecycle runs a whole session against the local docker
// daemon: up, metrics, apply a fix, reset, down. It is what proves the compose
// fallback really works on a developer machine, so it is gated on the
// environment variable rather than skipped silently.
func TestComposeFullLifecycle(t *testing.T) {
	if !dockerAvailable(t) {
		return
	}
	adapter := New(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	instance, err := adapter.Up(ctx, "integration-session", testSpec())
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer adapter.Down(context.Background(), "integration-session")

	if len(instance.Services) != 2 {
		t.Fatalf("services = %+v", instance.Services)
	}
	// The database is not published to the host (SQL goes through exec), so
	// only the app service carries a host endpoint.
	if instance.Services[0].Endpoint != "" {
		t.Fatalf("db should not publish a host port, got %q", instance.Services[0].Endpoint)
	}
	if instance.Services[1].Endpoint == "" {
		t.Fatal("no resolved endpoint for app")
	}

	samples, err := adapter.Metrics(ctx, "integration-session", "")
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("no metrics scraped")
	}

	// A fix must change what the environment reports, not just the compose
	// file: this is the assertion that caught `compose restart` ignoring env.
	if _, err := adapter.Apply(ctx, "integration-session", runtime.EnvPatch{
		Service: "app",
		Vars:    map[string]string{"POOL_LEAK": "true"},
		Restart: true,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	time.Sleep(3 * time.Second)
	metrics, err := adapter.Metrics(ctx, "integration-session", "app")
	if err != nil {
		t.Fatalf("Metrics after fix: %v", err)
	}
	var poolInUse float64
	found := false
	for _, sample := range metrics {
		if sample.Name == "app_db_pool_in_use" {
			poolInUse = sample.Value
			found = true
		}
	}
	if !found {
		t.Fatal("app_db_pool_in_use missing after the fix")
	}
	if poolInUse == 0 {
		t.Fatal("the recreated service still reports an empty pool: the env patch did not reach it")
	}

	if _, err := adapter.Reset(ctx, "integration-session"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	time.Sleep(2 * time.Second)
	after, err := adapter.Metrics(ctx, "integration-session", "app")
	if err != nil {
		t.Fatalf("Metrics after reset: %v", err)
	}
	for _, sample := range after {
		if sample.Name == "app_db_pool_in_use" && sample.Value == poolInUse {
			t.Fatalf("reset did not restore the baseline pool size (still %v)", sample.Value)
		}
	}
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

// TestServiceAnswersProbesHTTPNotJustTCP is the readiness contract behind a
// recreate: Docker's port proxy accepts a TCP connection before the process
// inside the container listens, so a verifier that starts measuring right
// after Apply used to see connection resets and call the environment broken.
// A service that declares a healthcheck must answer it over HTTP before the
// adapter reports the recreate as done.
func TestServiceAnswersProbesHTTPNotJustTCP(t *testing.T) {
	ctx := context.Background()

	serving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer serving.Close()
	servingHost := strings.TrimPrefix(serving.URL, "http://")

	check := &runtime.HealthcheckSpec{Path: "/healthz", Port: 80}
	if !serviceAnswers(ctx, servingHost, check) {
		t.Error("a service that answers its healthcheck was reported as down")
	}

	// A listening socket that speaks no HTTP is not a ready service: the TCP
	// fallback must not apply to a service that declares a healthcheck.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if serviceAnswers(ctx, listener.Addr().String(), check) {
		t.Error("a socket that accepts TCP but speaks no HTTP was reported as ready")
	}

	// Without a declared healthcheck the TCP dial stays the readiness probe:
	// a database publishes a port and answers nothing over HTTP.
	if !serviceAnswers(ctx, listener.Addr().String(), nil) {
		t.Error("a listening port was reported as down without a healthcheck to probe")
	}
}

func TestArtifactFilePathMapsToConfigDir(t *testing.T) {
	dir := t.TempDir()

	project := &session{
		id:        "session-1",
		docker:    "docker",
		workdir:   filepath.Join(dir, "session-1"),
		client:    NewClient(dir, "session-1"),
		spec:      runtime.Spec{},
		baseline:  runtime.Spec{},
		hostPorts: map[string]int{},
	}
	project.client = project.newClient()

	expectedPath := filepath.Join(dir, "session-1", "config", "app.conf")
	if got := project.artifactFilePath("/etc/app/app.conf"); got != expectedPath {
		t.Fatalf("artifact path = %q, want %q", got, expectedPath)
	}
}
