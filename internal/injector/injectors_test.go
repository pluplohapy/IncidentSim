package injector

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"incidentsim/internal/runtime"
)

// recordingEnv records what an injector did to the environment.
type recordingEnv struct {
	envVars  map[string]map[string]string
	sqlStmts []string
	execCmds []runtime.ExecRequest
}

func newRecordingEnv() *recordingEnv {
	return &recordingEnv{envVars: map[string]map[string]string{}}
}

func (e *recordingEnv) InstanceID() string { return "session-1" }

func (e *recordingEnv) Runtime() runtime.Adapter { return nil }

func (e *recordingEnv) SetEnv(ctx context.Context, service string, vars map[string]string) error {
	if _, ok := e.envVars[service]; !ok {
		e.envVars[service] = map[string]string{}
	}
	for key, value := range vars {
		e.envVars[service][key] = value
	}
	return nil
}

func (e *recordingEnv) ExecSQL(ctx context.Context, service, statement string) (runtime.ExecResult, error) {
	e.sqlStmts = append(e.sqlStmts, statement)
	return runtime.ExecResult{ExitCode: 0}, nil
}

// execEnv answers Exec with a scripted result, so the kill injector can be
// tested without a real process.
type execEnv struct {
	*recordingEnv
	result runtime.ExecResult
	err    error
}

func (e execEnv) Runtime() runtime.Adapter { return fakeRuntime{result: e.result, err: e.err} }

type fakeRuntime struct {
	result runtime.ExecResult
	err    error
}

func (f fakeRuntime) Name() string { return "fake" }

func (f fakeRuntime) Up(ctx context.Context, id string, spec runtime.Spec) (*runtime.Instance, error) {
	return nil, nil
}
func (f fakeRuntime) Down(ctx context.Context, id string) error { return nil }
func (f fakeRuntime) Exec(ctx context.Context, id string, req runtime.ExecRequest) (runtime.ExecResult, error) {
	return f.result, f.err
}
func (f fakeRuntime) Logs(ctx context.Context, id string, opts runtime.LogOptions) ([]runtime.LogLine, error) {
	return nil, nil
}
func (f fakeRuntime) Metrics(ctx context.Context, id, service string) ([]runtime.MetricSample, error) {
	return nil, nil
}
func (f fakeRuntime) Reset(ctx context.Context, id string) (*runtime.Instance, error) {
	return nil, nil
}
func (f fakeRuntime) Apply(ctx context.Context, id string, patch runtime.EnvPatch) (*runtime.Instance, error) {
	return nil, nil
}
func (f fakeRuntime) Deploy(ctx context.Context, id string, art runtime.Artifact, restart bool) (*runtime.Instance, error) {
	return nil, nil
}

func TestFeatureFlagInjectorSetsEnvironment(t *testing.T) {
	injector := NewFeatureFlagInjector()
	env := newRecordingEnv()
	fault := Fault{Kind: KindFeatureFlag, Target: "app", Options: map[string]any{
		"env": map[string]any{"LOG_LEVEL": "debug", "POOL_LEAK": true},
	}}
	if err := injector.Activate(context.Background(), env, fault); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	want := map[string]string{"LOG_LEVEL": "debug", "POOL_LEAK": "true"}
	if !reflect.DeepEqual(env.envVars["app"], want) {
		t.Fatalf("env = %v, want %v", env.envVars["app"], want)
	}
}

func TestFeatureFlagInjectorRefusesEmptyFault(t *testing.T) {
	injector := NewFeatureFlagInjector()
	err := injector.Activate(context.Background(), newRecordingEnv(), Fault{Kind: KindFeatureFlag, Target: "app"})
	if !errors.Is(err, ErrBadOptions) {
		t.Fatalf("error = %v, want ErrBadOptions", err)
	}
}

func TestSQLConfigInjectorRunsStatementsInOrder(t *testing.T) {
	injector := NewSQLConfigInjector()
	env := newRecordingEnv()
	fault := Fault{Kind: KindSQLConfig, Target: "db", Options: map[string]any{
		"statements": []any{
			"ALTER SYSTEM SET max_connections = 500",
			"SELECT pg_reload_conf()",
		},
	}}
	if err := injector.Activate(context.Background(), env, fault); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	want := []string{"ALTER SYSTEM SET max_connections = 500", "SELECT pg_reload_conf()"}
	if !reflect.DeepEqual(env.sqlStmts, want) {
		t.Fatalf("statements = %v, want %v", env.sqlStmts, want)
	}
}

func TestSQLConfigInjectorReportsFailedStatement(t *testing.T) {
	injector := NewSQLConfigInjector()
	env := newRecordingEnv()
	fault := Fault{Kind: KindSQLConfig, Target: "db", Options: map[string]any{
		"statements": []any{" "},
	}}
	if err := injector.Activate(context.Background(), env, fault); !errors.Is(err, ErrBadOptions) {
		t.Fatalf("error = %v, want ErrBadOptions", err)
	}
}

func TestKillInjectorStopsTheDeclaredProcess(t *testing.T) {
	injector := NewKillInjector()
	env := execEnv{recordingEnv: newRecordingEnv(), result: runtime.ExecResult{ExitCode: 0}}
	fault := Fault{Kind: KindKill, Target: "app", Options: map[string]any{"process": "worker", "signal": "KILL"}}
	if err := injector.Activate(context.Background(), env, fault); err != nil {
		t.Fatalf("Activate: %v", err)
	}
}

func TestKillInjectorDefaultsToPidOne(t *testing.T) {
	command := killCommand("", "TERM")
	if command != "kill -TERM 1" {
		t.Fatalf("kill command = %q, want kill -TERM 1", command)
	}
	if command := killCommand("worker", "KILL"); command != `pkill -KILL -f "worker"` {
		t.Fatalf("kill command = %q", command)
	}
}

func TestOptionsNormaliseYAMLScalars(t *testing.T) {
	options := NewOptions(map[string]any{
		"env":        map[string]any{"PORT": 8080, "DEBUG": true},
		"signal":     "TERM",
		"limit":      200.0,
		"process":    "worker",
		"statements": []any{"a", "b"},
		"delay":      "10s",
	})
	env := options.Env()
	if env["PORT"] != "8080" || env["DEBUG"] != "true" {
		t.Fatalf("env = %v", env)
	}
	if options.String("signal", "") != "TERM" {
		t.Fatalf("signal = %s", options.String("signal", ""))
	}
	if options.Int("limit", 0) != 200 {
		t.Fatalf("limit = %d", options.Int("limit", 0))
	}
	if options.String("missing", "fallback") != "fallback" {
		t.Fatal("missing option did not fall back")
	}
	if statements := options.StringSlice("statements"); len(statements) != 2 {
		t.Fatalf("statements = %v", statements)
	}
	if options.Duration("delay", "1s") != "10s" {
		t.Fatalf("delay = %s", options.Duration("delay", "1s"))
	}
	if !options.Has("signal") || options.Has("nope") {
		t.Fatal("Has reported the wrong presence")
	}
}

func TestDefaultRegistryCoversImplementableKinds(t *testing.T) {
	registry := DefaultRegistry()
	for _, kind := range []string{KindFeatureFlag, KindSQLConfig, KindKill} {
		if _, err := registry.Get(kind); err != nil {
			t.Fatalf("kind %s is not registered: %v", kind, err)
		}
	}
	// Unimplemented kinds must stay unregistered: a task that declares them
	// fails loudly instead of running without its fault.
	if _, err := registry.Get(KindNetwork); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("network error = %v, want ErrUnknownKind", err)
	}
}

func TestKillInjectorRestartDelayIsReachable(t *testing.T) {
	injector := NewKillInjector()
	if injector.RestartDelay <= 0 {
		t.Fatal("restart delay must be positive: a fault that never heals is a broken task")
	}
	_ = time.Second
}
