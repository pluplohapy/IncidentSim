package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// FakeAdapter is an in-memory Adapter used by core, injector and verifier
// tests. It records every call so tests can assert on the sequence of runtime
// operations without Docker or KVM.
type FakeAdapter struct {
	mu        sync.Mutex
	NameValue string
	Instance  *Instance
	// Calls records "Up", "Exec:app", "Logs:app", ... in call order.
	Calls []string
	// ExecFunc, when set, answers Exec calls instead of returning the zero
	// result. Tests use it to emulate a service.
	ExecFunc func(req ExecRequest) (ExecResult, error)
	// LogsFunc and MetricsFunc answer Logs and Metrics.
	LogsFunc    func(opts LogOptions) ([]LogLine, error)
	MetricsFunc func(service string) ([]MetricSample, error)
	// FailWith makes Up and Reset fail, to test error paths.
	FailWith error

	applied []EnvPatch
}

// NewFake returns a fake adapter that starts in the up state.
func NewFake(name string) *FakeAdapter {
	return &FakeAdapter{
		NameValue: name,
		Instance: &Instance{
			ID:      "fake-session",
			Engine:  name,
			Workdir: "/tmp/incidentsim-fake",
			Services: []Service{{
				Name:          "app",
				Endpoint:      "127.0.0.1:8080",
				ContainerPort: 8080,
				Healthcheck:   &HealthcheckSpec{Path: "/healthz", Port: 8080, IntervalSeconds: 1, TimeoutSeconds: 1},
			}},
			StartedAt: time.Now(),
		},
	}
}

func (f *FakeAdapter) Name() string { return f.NameValue }

func (f *FakeAdapter) Up(ctx context.Context, sessionID string, spec Spec) (*Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "Up")
	if f.FailWith != nil {
		return nil, f.FailWith
	}
	f.Instance.ID = sessionID
	out := *f.Instance
	out.Services = append([]Service(nil), f.Instance.Services...)
	return &out, nil
}

func (f *FakeAdapter) Down(ctx context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "Down")
	return f.FailWith
}

func (f *FakeAdapter) Exec(ctx context.Context, sessionID string, req ExecRequest) (ExecResult, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, "Exec:"+req.Service)
	exec := f.ExecFunc
	f.mu.Unlock()
	if exec != nil {
		return exec(req)
	}
	return ExecResult{ExitCode: 0, Stdout: "ok", Duration: time.Millisecond}, nil
}

func (f *FakeAdapter) Logs(ctx context.Context, sessionID string, opts LogOptions) ([]LogLine, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, "Logs:"+opts.Service)
	logs := f.LogsFunc
	f.mu.Unlock()
	if logs != nil {
		return logs(opts)
	}
	return []LogLine{{Service: opts.Service, Text: "line 1"}}, nil
}

func (f *FakeAdapter) Metrics(ctx context.Context, sessionID string, service string) ([]MetricSample, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, "Metrics:"+service)
	metrics := f.MetricsFunc
	f.mu.Unlock()
	if metrics != nil {
		return metrics(service)
	}
	return []MetricSample{{Service: service, Name: "app_errors_total", Value: 0, At: time.Now()}}, nil
}

func (f *FakeAdapter) Reset(ctx context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "Reset")
	f.applied = nil
	return f.FailWith
}

func (f *FakeAdapter) Apply(ctx context.Context, sessionID string, patch EnvPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "Apply:"+patch.Service)
	f.applied = append(f.applied, patch)
	return f.FailWith
}

// AppliedPatches returns the patches seen by Apply, oldest first.
func (f *FakeAdapter) AppliedPatches() []EnvPatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]EnvPatch(nil), f.applied...)
}

// LastEnv returns the effective value of an environment variable of a service,
// as seen through Apply calls. It models what a policy check inspects.
func (f *FakeAdapter) LastEnv(service, key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.applied) - 1; i >= 0; i-- {
		patch := f.applied[i]
		if patch.Service != service {
			continue
		}
		if value, ok := patch.Vars[key]; ok {
			return value, value != ""
		}
	}
	return "", false
}

var _ Adapter = (*FakeAdapter)(nil)

// String renders the recorded calls for readable test failures.
func (f *FakeAdapter) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprint(f.Calls)
}
