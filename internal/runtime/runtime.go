// Package runtime defines the contract between the core and an isolation
// engine: Firecracker in production, Docker Compose for development.
//
// The interface is deliberately small and synchronous. Everything the core
// needs from an environment is expressed as a method here, so a new engine is
// a single file that implements these six verbs.
package runtime

import (
	"context"
	"errors"
	"time"
)

// Errors returned by every Adapter implementation. The core branches on them
// to decide what is a retry, what is a bug in the task and what is a missing
// capability of the engine.
var (
	// ErrNotRunning means the instance exists but is not up.
	ErrNotRunning = errors.New("instance is not running")
	// ErrNotFound means the instance is unknown to the adapter.
	ErrNotFound = errors.New("instance not found")
	// ErrUnsupported means the engine cannot serve this operation.
	ErrUnsupported = errors.New("operation unsupported by this runtime")
	// ErrTimeout means an operation exceeded its deadline.
	ErrTimeout = errors.New("operation timed out")
)

// Instance is a live environment of one task session.
type Instance struct {
	// ID is the session id, unique per start.
	ID string
	// Engine is the adapter that produced the instance.
	Engine string
	// Services are the task services, in manifest order.
	Services []Service
	// Workdir is where the adapter keeps its state (compose project dir,
	// firecracker socket dir). It is the only place the adapter writes.
	Workdir string
	// StartedAt is the moment Up returned successfully.
	StartedAt time.Time
}

// Service is one process of the environment as seen from the host.
type Service struct {
	Name string
	// Endpoint is host:port of the service, reachable from the core. It is
	// what the replay runner and the http checks talk to.
	Endpoint string
	// ContainerPort is the port the service listens on inside the environment.
	ContainerPort int
	// PID is the engine level pid (container or microVM), best effort.
	PID int
	// Healthcheck is the manifest healthcheck of the service, if any.
	Healthcheck *HealthcheckSpec
}

// EnvPatch is a configuration change the participant applies. It is the only
// write path into the environment outside of restart: the fix must be
// expressible as a config change, otherwise the task cannot be graded.
type EnvPatch struct {
	Service string
	// Vars are the variables to set. An empty value means "unset".
	Vars map[string]string
	// Restart applies the patch and restarts the service, which is how a
	// participant deploys a fix in this simulator.
	Restart bool
	// Command is an optional command executed inside the service before
	// restart, e.g. a SQL statement for db-level tasks.
	Command []string
}

// ExecRequest asks a service to run a command.
type ExecRequest struct {
	Service string
	Command []string
	// Env is added to the command environment.
	Env map[string]string
	// Timeout defaults to 30s in the adapters.
	Timeout time.Duration
	// Stdin is optional input for the command.
	Stdin []byte
}

// ExecResult is the outcome of an Exec.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// LogOptions narrows a Logs call.
type LogOptions struct {
	// Service is required.
	Service string
	// Since keeps only lines newer than this instant.
	Since time.Time
	// Until keeps only lines older than this instant.
	Until time.Time
	// Grep keeps only lines containing this substring.
	Grep string
	// Tail limits the number of returned lines, 0 means all.
	Tail int
}

// LogLine is one line of a service log with a best effort timestamp.
type LogLine struct {
	Service   string
	Timestamp time.Time
	Stream    string // "stdout" or "stderr"
	Text      string
}

// MetricSample is one observation of a named metric.
type MetricSample struct {
	Service string
	Name    string
	// Value is the numeric value of the sample.
	Value float64
	// Labels are optional dimensions of the sample.
	Labels map[string]string
	At     time.Time
}

// Adapter is the runtime contract. Implementations live in
// internal/adapters/compose and internal/adapters/firecracker.
//
// Every method takes a session id: the core owns the lifecycle and never
// reuses an id, so an adapter can treat the id as a primary key.
type Adapter interface {
	// Name returns the engine identifier, e.g. "compose" or "firecracker".
	Name() string

	// Up provisions the environment and starts every service of the task.
	// It must be idempotent per session id: calling it twice for the same id
	// returns the same instance without leaking a second environment.
	Up(ctx context.Context, sessionID string, spec Spec) (*Instance, error)

	// Down stops the environment and releases host resources. It is safe to
	// call for an unknown session id.
	Down(ctx context.Context, sessionID string) error

	// Exec runs a command inside a service.
	Exec(ctx context.Context, sessionID string, req ExecRequest) (ExecResult, error)

	// Logs returns the collected log lines of a service.
	Logs(ctx context.Context, sessionID string, opts LogOptions) ([]LogLine, error)

	// Metrics returns the latest metrics of a service.
	Metrics(ctx context.Context, sessionID string, service string) ([]MetricSample, error)

	// Reset restores the baseline state of the environment. Firecracker
	// restores the baseline snapshot; compose does a down/up. The session id
	// stays valid after a reset. It returns the refreshed instance because a
	// reset can move published ports.
	Reset(ctx context.Context, sessionID string) (*Instance, error)

	// Apply pushes a configuration change into a running environment. It
	// returns the refreshed instance for the same reason: recreating a
	// service can change where it is reachable from.
	Apply(ctx context.Context, sessionID string, patch EnvPatch) (*Instance, error)
}

// Spec is the manifest view an adapter needs to build an environment. It is a
// projection of manifest.Spec, decoupled so that adapters do not import the
// manifest package and schema changes do not ripple into engines.
type Spec struct {
	Services []ServiceSpec
	// Snapshot names the baseline artefact the engine restores on Reset.
	Snapshot string
	// CPUCount and MemoryMiB size the environment.
	CPUCount  int
	MemoryMiB int
	// FaultEnv holds the variables the injector wants present at boot.
	FaultEnv map[string]string
}

// ServiceSpec is one service to run.
type ServiceSpec struct {
	Name        string
	Image       string
	Env         map[string]string
	Ports       []int
	Command     []string
	Healthcheck *HealthcheckSpec
}

// HealthcheckSpec is how the adapter decides a service is usable.
type HealthcheckSpec struct {
	Path            string
	Port            int
	IntervalSeconds int
	TimeoutSeconds  int
}

// WaitForHealth polls every service healthcheck until the deadline. It is a
// helper shared by the adapters so that Up returns only when the environment
// is actually usable.
func WaitForHealth(ctx context.Context, instance *Instance, deadline time.Duration) error {
	for _, service := range instance.Services {
		check := service.Healthcheck
		if check == nil {
			continue
		}
		if err := waitEndpoint(ctx, service, check, deadline); err != nil {
			return err
		}
	}
	return nil
}

// DefaultTimeouts used by the adapters, kept here so both engines behave
// identically in a session.
const (
	// DefaultExecTimeout bounds every Exec call.
	DefaultExecTimeout = 30 * time.Second
	// DefaultUpTimeout bounds the whole environment bootstrap.
	DefaultUpTimeout = 5 * time.Minute
	// DefaultResetTimeout bounds a reset.
	DefaultResetTimeout = 2 * time.Minute
)
