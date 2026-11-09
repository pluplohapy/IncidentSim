package verifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"incidentsim/internal/runtime"
)

func TestReplayRunnerCountsErrorsAndLatencies(t *testing.T) {
	requests, failures := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests%3 == 0 {
			failures++
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	runner := &ReplayRunner{Now: time.Now}
	result, err := runner.Replay(context.Background(), server.URL, []Request{
		{Name: "orders", Path: "/api/orders", Weight: 1},
	}, ReplayConfig{Duration: "300ms", RPS: 50})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result.Requests == 0 {
		t.Fatal("no requests were replayed")
	}
	if result.Errors == 0 {
		t.Fatal("a 5xx under replay was not counted as an error")
	}
	if result.LatencyP50 <= 0 {
		t.Fatalf("latency p50 = %v", result.LatencyP50)
	}
	if result.LatencyP99 < result.LatencyP50 {
		t.Fatalf("p99 %v is below p50 %v", result.LatencyP99, result.LatencyP50)
	}
	if result.String() == "" {
		t.Fatal("empty replay summary")
	}
}

func TestReplayRunnerFailsOnUnusableDuration(t *testing.T) {
	runner := &ReplayRunner{Now: time.Now}
	_, err := runner.Replay(context.Background(), "127.0.0.1:1", nil, ReplayConfig{Duration: "banana"})
	if err == nil {
		t.Fatal("a malformed duration was accepted")
	}
}

func TestReplayRunnerFailsWithoutEndpoint(t *testing.T) {
	runner := &ReplayRunner{Now: time.Now}
	_, err := runner.Replay(context.Background(), "", nil, ReplayConfig{Duration: "1s"})
	if err == nil {
		t.Fatal("an empty endpoint was accepted")
	}
}

func TestReplayRunnerStopsWhenContextEnds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	runner := &ReplayRunner{Now: time.Now}
	_, err := runner.Replay(ctx, server.URL, nil, ReplayConfig{Duration: "30s"})
	if err == nil {
		t.Fatal("a cancelled replay reported success")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("replay kept running for %v after cancellation", elapsed)
	}
}

func TestLoadReplayProfileValidatesItsContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workloads.yaml")
	content := `
profile: fixture
duration: 30s
rps: 20
requests:
  - name: orders
    method: GET
    path: /api/orders
    weight: 5
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := LoadReplayProfile(dir, "workloads.yaml")
	if err != nil {
		t.Fatalf("LoadReplayProfile: %v", err)
	}
	if profile.Profile != "fixture" || len(profile.Requests) != 1 {
		t.Fatalf("profile = %+v", profile)
	}
	if profile.Requests[0].Weight != 5 {
		t.Fatalf("weight = %d", profile.Requests[0].Weight)
	}

	// A profile that cannot drive a replay is a task bug, so it must fail on
	// load rather than half way through a verification.
	for name, broken := range map[string]string{
		"no-duration": "profile: x\nrps: 1\nrequests:\n  - path: /a\n",
		"no-requests": "profile: x\nduration: 1s\nrps: 1\n",
		"bad-rps":     "profile: x\nduration: 1s\nrps: 0\nrequests:\n  - path: /a\n",
	} {
		brokenPath := filepath.Join(dir, name+".yaml")
		os.WriteFile(brokenPath, []byte(broken), 0o600)
		if _, err := LoadReplayProfile(dir, name+".yaml"); err == nil {
			t.Errorf("broken profile %s was accepted", name)
		}
	}
}

func TestManifestVerifierRunsReplayBeforeGates(t *testing.T) {
	// A replay that reports load is part of the report: a verification that
	// measures nothing is not evidence of anything.
	verifier := NewManifestVerifier()
	verifier.ReplayRunner = &ReplayRunner{Now: time.Now}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "p.yaml"), []byte("profile: p\nduration: 200ms\nrps: 20\nrequests:\n  - path: /a\n"), 0o600)
	verifier.ProjectDir = dir

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	env := FakeEnvironment{
		Services:  []runtime.Service{{Name: "app", Endpoint: server.Listener.Addr().String()}},
		EnvValues: map[string]map[string]string{"app": {"LOG_LEVEL": "info"}},
	}
	report, err := verifier.Verify(context.Background(), env, "s", "t", CheckSet{
		Correctness: []Check{{ID: "ok", Kind: CheckHTTP, Service: "app", Path: "/", Expect: &Expectation{Status: intPtr(200)}}},
		Quality:     []Check{{ID: "env-ok", Kind: CheckPolicy, Service: "app", Rule: RuleEnvFlagEquals, Query: "LOG_LEVEL", Expect: &Expectation{Equals: strPtr("info")}}},
		Replay:      &ReplayConfig{Profile: "p.yaml", Duration: "200ms", RPS: 20},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.Replay == "" {
		t.Fatal("report has no replay summary")
	}
	if !report.Passed() {
		t.Fatalf("verdict = %s, want pass\n%s", report.Verdict, report.String())
	}
}

func intPtr(value int) *int       { return &value }
func strPtr(value string) *string { return &value }
