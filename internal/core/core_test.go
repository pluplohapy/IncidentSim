package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"incidentsim/internal/manifest"
	"incidentsim/internal/runtime"
	"incidentsim/internal/verifier"
)

// testManifest is a minimal task good enough to exercise the lifecycle.
const testManifest = `
apiVersion: incidentsim/v1
kind: Incident
metadata:
  id: fixture-task
  title: "Фикстура жизненного цикла"
  level: config
spec:
  runtime:
    engine: compose
  services:
    - name: app
      image: incidentsim/workload:dev
      env:
        LOG_LEVEL: debug
      ports: [8080]
      healthcheck:
        path: /healthz
        port: 8080
  symptoms:
    - id: errors-5xx
      observation: "запросы отвечают 503"
  hints:
    - level: 1
      text: "Посмотрите уровень логирования сервиса app"
  fault:
    type: feature_flag
    target: app
    options:
      env:
        LOG_LEVEL: debug
  verify:
    correctness:
      - id: healthz-200
        type: http
        service: app
        path: /healthz
        expect:
          status: 200
    quality:
      - id: log-level-info
        type: policy
        service: app
        rule: env_flag_equals
        query: LOG_LEVEL
        expect:
          equals: info
        reason: "debug-логи в проде запрещены политикой"
  root_cause:
    summary: "Включён debug-уровень логирования."
    layer: config
  reference_solution:
    summary: "Понизить уровень логирования до info."
`

// newTestOrchestrator builds an orchestrator over a fake adapter and an
// httptest server that stands in for the task service.
func newTestOrchestrator(t *testing.T) (*Orchestrator, *runtime.FakeAdapter, func()) {
	t.Helper()

	catalogDir := t.TempDir()
	writeFile(t, catalogDir+"/fixture-task.yaml", testManifest)
	config := DefaultConfig()
	config.Catalog = catalogDir
	config.State = t.TempDir()

	catalog, err := NewCatalog(config.Catalog)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	fake := runtime.NewFake("compose")
	fake.ExecFunc = func(req runtime.ExecRequest) (runtime.ExecResult, error) {
		// The fake reports the environment the patches have produced, which is
		// what a policy check reads to see whether a flag is still on.
		var text string
		for _, patch := range fake.AppliedPatches() {
			if patch.Service != req.Service {
				continue
			}
			for key, value := range patch.Vars {
				text += key + "=" + value + "\n"
			}
		}
		return runtime.ExecResult{Stdout: text}, nil
	}
	orchestrator, err := New(config, catalog, map[string]runtime.Adapter{"compose": fake}, nil, verifier.NewManifestVerifier())
	if err != nil {
		t.Fatalf("orchestrator: %v", err)
	}
	teardown := func() { fake.Down(context.Background(), "") }
	return orchestrator, fake, teardown
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestStartActivatesDeclaredFault(t *testing.T) {
	orchestrator, fake, teardown := newTestOrchestrator(t)
	defer teardown()

	ctx := context.Background()
	session, err := orchestrator.Start(ctx, "fixture-task")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if session.State != StateRunning {
		t.Fatalf("state = %s, want %s", session.State, StateRunning)
	}
	// The fault must reach the runtime as an environment patch, otherwise the
	// participant gets a healthy environment and nothing to diagnose.
	patches := fake.AppliedPatches()
	if len(patches) == 0 {
		t.Fatal("no fault was applied to the environment")
	}
	if patches[0].Vars["LOG_LEVEL"] != "debug" {
		t.Fatalf("fault env = %v", patches[0].Vars)
	}
	events := session.journal.Events()
	if !hasEvent(events, EventFaultActivated) {
		t.Fatalf("journal has no fault event: %v", events)
	}
}

func TestVerifyGatesRunIndependently(t *testing.T) {
	orchestrator, fake, teardown := newTestOrchestrator(t)
	defer teardown()

	ctx := context.Background()
	session, err := orchestrator.Start(ctx, "fixture-task")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The fake adapter serves the healthcheck endpoint from a test server so
	// the correctness gate has something real to call.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	fake.Instance.Services[0].Endpoint = server.Listener.Addr().String()
	session.mu.Lock()
	session.instance.Services[0].Endpoint = server.Listener.Addr().String()
	session.mu.Unlock()

	// The environment works, but the flag that causes the fault is still on:
	// correctness passes and quality fails.
	report, err := orchestrator.Verify(ctx, session.ID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.Correctness.Verdict != verifier.VerdictPass {
		t.Fatalf("correctness = %s, want pass (broken environment? no: this is the masked-symptom case)", report.Correctness.Verdict)
	}
	if report.Quality.Verdict != verifier.VerdictFail {
		t.Fatalf("quality = %s, want fail: the flag is still debug", report.Quality.Verdict)
	}
	if report.Verdict != verifier.VerdictPartial {
		t.Fatalf("verdict = %s, want partial_fix", report.Verdict)
	}

	// The same environment after a real fix passes both gates.
	if err := orchestrator.Apply(ctx, session.ID, runtime.EnvPatch{
		Service: "app", Vars: map[string]string{"LOG_LEVEL": "info"}, Restart: true,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fake.Instance.Services[0].Endpoint = server.Listener.Addr().String()
	report, err = orchestrator.Verify(ctx, session.ID)
	if err != nil {
		t.Fatalf("Verify after fix: %v", err)
	}
	if report.Verdict != verifier.VerdictPass {
		t.Fatalf("verdict after fix = %s, want pass\n%s", report.Verdict, report.String())
	}
}

func TestResetClearsAppliedFixes(t *testing.T) {
	orchestrator, fake, teardown := newTestOrchestrator(t)
	defer teardown()

	ctx := context.Background()
	session, err := orchestrator.Start(ctx, "fixture-task")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := orchestrator.Apply(ctx, session.ID, runtime.EnvPatch{Service: "app", Vars: map[string]string{"LOG_LEVEL": "info"}}); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Reset(ctx, session.ID); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if snapshot := session.Snapshot(); len(snapshot.AppliedFixes) != 0 {
		t.Fatalf("reset kept fixes: %v", snapshot.AppliedFixes)
	}
	// What the simulator broke is re-created, not left behind: after a reset
	// the fault artifacts describe the current, broken environment.
	if snapshot := session.Snapshot(); len(snapshot.FaultArtifacts) != 1 {
		t.Fatalf("fault artifacts after reset = %v, want exactly the re-activated fault", snapshot.FaultArtifacts)
	}
	// A reset means "back to the incident": the declared fault must be active
	// again, otherwise the participant gets a healthy environment and nothing
	// left to diagnose.
	// Exactly one patch remains: the re-activated fault. The participant's fix
	// is gone, and the fault the manifest declares is back.
	patches := fake.AppliedPatches()
	if len(patches) != 1 {
		t.Fatalf("after reset the runtime saw %d patches, want 1 (the re-activated fault): %v", len(patches), patches)
	}
	if patches[0].Vars["LOG_LEVEL"] != "debug" {
		t.Fatalf("re-activated fault = %v, want LOG_LEVEL=debug", patches[0].Vars)
	}
	events := session.journal.Events()
	resetAt := -1
	for i, event := range events {
		if event.Type == EventReset {
			resetAt = i
		}
	}
	if resetAt < 0 || resetAt+1 >= len(events) || events[resetAt+1].Type != EventFaultActivated {
		t.Fatalf("reset did not re-activate the fault: %v", eventTypes(events))
	}
}

func eventTypes(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.Type)
	}
	return out
}

func TestHintIsChargedToTheSession(t *testing.T) {
	orchestrator, _, teardown := newTestOrchestrator(t)
	defer teardown()

	session, err := orchestrator.Start(context.Background(), "fixture-task")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	hint, found, err := orchestrator.Hint(session.ID, 0)
	if err != nil || !found {
		t.Fatalf("Hint = %v, %v, %v", hint, found, err)
	}
	if session.Snapshot().HintsUsed != 1 {
		t.Fatalf("hints used = %d, want 1", session.Snapshot().HintsUsed)
	}
	// Beyond the last level the service says so instead of inventing a hint.
	last, found, err := orchestrator.Hint(session.ID, 99)
	if err != nil {
		t.Fatal(err)
	}
	if found || !strings.Contains(last.Text, "закончились") {
		t.Fatalf("hint beyond the last level = %q, found=%v", last.Text, found)
	}
}

func TestJournalPersistsEvents(t *testing.T) {
	orchestrator, _, teardown := newTestOrchestrator(t)
	defer teardown()

	session, err := orchestrator.Start(context.Background(), "fixture-task")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	journal, err := LoadJournal(orchestrator.Config.JournalDir(), session.ID)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	events := journal.Events()
	if len(events) < 2 {
		t.Fatalf("persisted %d events, want at least 2", len(events))
	}
	if events[0].Type != EventSessionStarted {
		t.Fatalf("first event = %s, want %s", events[0].Type, EventSessionStarted)
	}
	for i, event := range events {
		if event.Seq != i+1 {
			t.Fatalf("event %d has seq %d", i, event.Seq)
		}
	}
	if journal.Path() == "" {
		t.Fatal("journal has no file path")
	}
}

func TestStopTearsEnvironmentDown(t *testing.T) {
	orchestrator, fake, teardown := newTestOrchestrator(t)
	defer teardown()

	session, err := orchestrator.Start(context.Background(), "fixture-task")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := orchestrator.Stop(context.Background(), session.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := orchestrator.Sessions.Get(session.ID); err == nil {
		t.Fatal("session still registered after stop")
	}
	if calls := fake.Calls; calls[len(calls)-1] != "Down" {
		t.Fatalf("last runtime call = %s, want Down", calls[len(calls)-1])
	}
}

func TestParseEnvPatchAllowsEmptySet(t *testing.T) {
	// --set is optional: an empty vars map is a valid patch at this layer.
	// The caller rejects a fully empty participant action before reaching
	// ParseEnvPatch.
	patch, err := ParseEnvPatch("app", nil, false)
	if err != nil {
		t.Fatalf("empty --set list rejected: %v", err)
	}
	if patch.Restart {
		t.Fatalf("patch = %+v, want restart=false", patch)
	}
	if _, err := ParseEnvPatch("app", []string{"nonsense"}, true); err == nil {
		t.Fatal("malformed --set accepted")
	}
	patch, err = ParseEnvPatch("app", []string{"LOG_LEVEL=info"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if patch.Vars["LOG_LEVEL"] != "info" || patch.Restart {
		t.Fatalf("patch = %+v", patch)
	}
}

func TestCatalogRejectsUnknownTask(t *testing.T) {
	orchestrator, _, teardown := newTestOrchestrator(t)
	defer teardown()
	if _, err := orchestrator.Start(context.Background(), "nope"); err == nil {
		t.Fatal("unknown task started")
	}
}

func hasEvent(events []Event, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func TestChecksProjectionCarriesReplayConfiguration(t *testing.T) {
	m, err := manifest.Parse([]byte(testManifest))
	if err != nil {
		t.Fatal(err)
	}
	m.Spec.Replay = &manifest.ReplaySpec{Profile: "workloads/x.yaml", Duration: "30s", RPS: 10}
	set := Checks(m)
	if set.Replay == nil || set.Replay.RPS != 10 {
		t.Fatalf("replay config = %+v", set.Replay)
	}
	if len(set.Correctness) != 1 || len(set.Quality) != 1 {
		t.Fatalf("checks = %d/%d", len(set.Correctness), len(set.Quality))
	}
	if set.Correctness[0].Gate != verifier.GateCorrectness {
		t.Fatalf("correctness check gate = %s", set.Correctness[0].Gate)
	}
}

func TestDefaultConfigStateLayout(t *testing.T) {
	config := DefaultConfig()
	if config.Listen != "127.0.0.1:8747" {
		t.Fatalf("listen = %s", config.Listen)
	}
	dir := config.JournalDir()
	if !strings.HasSuffix(dir, "journal") {
		t.Fatalf("journal dir = %s", dir)
	}
	loaded, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("missing config file must fall back to defaults: %v", err)
	}
	if loaded.Engine != "compose" {
		t.Fatalf("engine = %s", loaded.Engine)
	}
	_ = time.Now()
}

// editableTaskManifest is a manifest whose editable list is just app/main.go.
const editableTaskManifest = `
apiVersion: incidentsim/v1
kind: Incident
metadata:
  id: editable-task
  title: "Редактируемые файлы"
  level: code
spec:
  runtime:
    engine: compose
  services:
    - name: app
      image: incidentsim/workload:dev
      env:
        LOG_LEVEL: debug
      ports: [8080]
      healthcheck:
        path: /healthz
        port: 8080
  editable: [app/main.go]
  verify:
    correctness:
      - id: healthz-200
        type: http
        service: app
        path: /healthz
        expect:
          status: 200
    quality:
      - id: pool-ok
        type: metric
        service: app
        query: app_db_pool_in_use
        duration: 30s
        expect:
          max: 5
        reason: "пул не должен быть исчерпан"
      - id: pool-not-maxed
        type: exec
        service: app
        command: ["sh", "-c", "true"]
        reason: "artefact check required for quality gate"
  root_cause:
    summary: "утечка соединений в пуле из-за отсутствия rows.Err"
    layer: code
  reference_solution:
    summary: "добавить defer rows.Close() и rows.Err() в обработчик заказов"
`

// scenarioDirForEditable creates a scenario workspace under dir with the given files
// and returns the orchestrator wired to load editable-task from dir.
func scenarioDirForEditable(t *testing.T, dir string, files map[string]string) *Orchestrator {
	t.Helper()
	catalogDir := filepath.Join(dir, "manifests")
	scenarioDir := filepath.Join(dir, "scenarios", "editable-task", "workspace")
	if err := os.MkdirAll(catalogDir, 0o755); err != nil {
		t.Fatalf("mkdir catalog: %v", err)
	}
	if err := os.WriteFile(filepath.Join(catalogDir, "editable-task.yaml"), []byte(editableTaskManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(scenarioDir, name)), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(scenarioDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	config := DefaultConfig()
	config.Catalog = catalogDir
	config.State = filepath.Join(dir, "state")
	catalog, err := NewCatalog(config.Catalog)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	orchestrator, err := New(config, catalog, map[string]runtime.Adapter{"compose": runtime.NewFake("compose")}, nil, verifier.NewManifestVerifier())
	if err != nil {
		t.Fatalf("orchestrator: %v", err)
	}
	return orchestrator
}

func TestValidateEditableFilesTable(t *testing.T) {
	base := []struct {
		name    string
		files   map[string]string // scenario files
		dst     map[string]string // session workspace files (overrides scenario)
		delete  []string          // files present in scenario but removed from session
		wantErr bool
	}{
		{
			name:    "no changes is ok",
			files:   map[string]string{"app/main.go": "package main\n", "Dockerfile": "FROM x\n"},
			wantErr: false,
		},
		{
			name:    "changed editable is ok",
			files:   map[string]string{"app/main.go": "package main\n", "Dockerfile": "FROM x\n"},
			dst:     map[string]string{"app/main.go": "package main // fixed\n"},
			wantErr: false,
		},
		{
			name:    "changed dockerfile fails",
			files:   map[string]string{"app/main.go": "package main\n", "Dockerfile": "FROM x\n"},
			dst:     map[string]string{"Dockerfile": "FROM y\n"},
			wantErr: true,
		},
		{
			name:    "added extra file fails",
			files:   map[string]string{"app/main.go": "package main\n", "Dockerfile": "FROM x\n"},
			dst:     map[string]string{"app/main.go": "package main\n", "Dockerfile": "FROM x\n", "evil.go": "package main\n"},
			wantErr: true,
		},
		{
			name:    "deleted non-editable file fails",
			files:   map[string]string{"app/main.go": "package main\n", "Dockerfile": "FROM x\n", "go.mod": "module x\n"},
			delete:  []string{"Dockerfile"},
			wantErr: true,
		},
	}

	for _, tc := range base {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			orchestrator := scenarioDirForEditable(t, dir, tc.files)
			dst := filepath.Join(dir, "state", "sessions", "editable-task-1-000000", "workspace")
			if err := os.MkdirAll(dst, 0o755); err != nil {
				t.Fatalf("mkdir session workspace: %v", err)
			}
			// Seed session workspace from scenario, then apply overrides/deletions.
			src := filepath.Join(dir, "scenarios", "editable-task", "workspace")
			if err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(src, path)
				if err != nil {
					return err
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dst, rel)), 0o755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
			}); err != nil {
				t.Fatalf("seed workspace: %v", err)
			}
			for name, content := range tc.dst {
				if err := os.WriteFile(filepath.Join(dst, name), []byte(content), 0o644); err != nil {
					t.Fatalf("write dst %s: %v", name, err)
				}
			}
			for _, name := range tc.delete {
				if err := os.Remove(filepath.Join(dst, name)); err != nil && !os.IsNotExist(err) {
					t.Fatalf("remove %s: %v", name, err)
				}
			}
			// Build a session pointing at the scenario and the seeded workspace.
			task, err := orchestrator.Catalog.Load("editable-task")
			if err != nil {
				t.Fatalf("load task: %v", err)
			}
			session := &Session{
				ID:           "editable-task-1-000000",
				TaskID:       "editable-task",
				manifest:     task,
				WorkspaceDir: dst,
			}
			err = orchestrator.validateEditableFiles(session)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateEditableFiles error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateFilePathRejectsTraversal(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "normal relative", path: "app/main.go", wantErr: false},
		{name: "subdir relative", path: "sub/app/main.go", wantErr: false},
		{name: "absolute", path: "/etc/passwd", wantErr: true},
		{name: "dotdot segment", path: "../etc/passwd", wantErr: true},
		{name: "dotdot mid", path: "app/../../etc/passwd", wantErr: true},
		{name: "dotdot escaped", path: "../../etc/passwd", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFilePath(tc.path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateFilePath(%q) = %v, wantErr = %v", tc.path, err, tc.wantErr)
			}
		})
	}
}

var _ = runtime.ErrNotFound

var _ = manifest.APIVersion
