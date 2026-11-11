package verifier

import (
	"context"

	"incidentsim/internal/runtime"
)

// FakeEnvironment is an in-memory verifier.Environment for tests. It lets a
// check be exercised without an engine, which is what makes the
// "distractor passes correctness, fails quality" test cheap.
type FakeEnvironment struct {
	InstanceIDValue string
	// EnvValues maps service name to its effective environment.
	EnvValues map[string]map[string]string
	// SQLRows maps service name to the rows SQL() returns.
	SQLRows map[string][]map[string]string
	// Services is what Instance() returns, used by the replay runner.
	Services []runtime.Service
	// Err makes every accessor fail.
	Err error
	// ExecFunc answers Exec calls. If nil, Exec returns a zero result.
	ExecFunc func(req runtime.ExecRequest) (runtime.ExecResult, error)
}

// InstanceID implements Environment.
func (f FakeEnvironment) InstanceID() string { return f.InstanceIDValue }

// Runtime implements Environment.
func (f FakeEnvironment) Runtime() runtime.Adapter { return &f }

// Instance implements Environment.
func (f FakeEnvironment) Instance() *runtime.Instance {
	return &runtime.Instance{ID: f.InstanceIDValue, Services: f.Services}
}

// Env implements Environment.
func (f FakeEnvironment) Env(ctx context.Context, service string) (map[string]string, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.EnvValues[service], nil
}

// SQL implements Environment.
func (f FakeEnvironment) SQL(ctx context.Context, service, statement string) ([]map[string]string, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.SQLRows[service], nil
}

// Name implements runtime.Adapter.
func (f FakeEnvironment) Name() string { return "fake" }

// Up implements runtime.Adapter.
func (f FakeEnvironment) Up(ctx context.Context, sessionID string, spec runtime.Spec) (*runtime.Instance, error) {
	return nil, runtime.ErrUnsupported
}

// Down implements runtime.Adapter.
func (f FakeEnvironment) Down(ctx context.Context, sessionID string) error {
	return runtime.ErrUnsupported
}

// Exec implements runtime.Adapter.
func (f FakeEnvironment) Exec(ctx context.Context, sessionID string, req runtime.ExecRequest) (runtime.ExecResult, error) {
	if f.ExecFunc != nil {
		return f.ExecFunc(req)
	}
	return runtime.ExecResult{}, nil
}

// Logs implements runtime.Adapter.
func (f FakeEnvironment) Logs(ctx context.Context, sessionID string, opts runtime.LogOptions) ([]runtime.LogLine, error) {
	return nil, runtime.ErrUnsupported
}

// Metrics implements runtime.Adapter.
func (f FakeEnvironment) Metrics(ctx context.Context, sessionID string, service string) ([]runtime.MetricSample, error) {
	return nil, runtime.ErrUnsupported
}

// Reset implements runtime.Adapter.
func (f FakeEnvironment) Reset(ctx context.Context, sessionID string) (*runtime.Instance, error) {
	return nil, runtime.ErrUnsupported
}

// Apply implements runtime.Adapter.
func (f FakeEnvironment) Apply(ctx context.Context, sessionID string, patch runtime.EnvPatch) (*runtime.Instance, error) {
	return nil, runtime.ErrUnsupported
}

// Deploy implements runtime.Adapter.
func (f FakeEnvironment) Deploy(ctx context.Context, sessionID string, art runtime.Artifact, restart bool) (*runtime.Instance, error) {
	return nil, runtime.ErrUnsupported
}

var _ Environment = FakeEnvironment{}
var _ runtime.Adapter = FakeEnvironment{}
