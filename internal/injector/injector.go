// Package injector activates faults declared by a manifest.
//
// A manifest declares what is broken; an injector knows how to break it in a
// concrete environment. The split keeps the catalog free of engine details:
// feature_flag works the same way on compose and on a Firecracker microVM.
package injector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"incidentsim/internal/runtime"
)

// Kinds of faults an injector can activate. They mirror the manifest schema.
const (
	KindFeatureFlag = "feature_flag"
	KindSQLConfig   = "sql_config"
	KindKill        = "kill"
	KindNetwork     = "network"
	KindStress      = "stress"
)

// Kinds lists the supported fault kinds in a stable order.
func Kinds() []string {
	return []string{KindFeatureFlag, KindSQLConfig, KindKill, KindNetwork, KindStress}
}

// Errors of the injector subsystem.
var (
	// ErrUnknownKind means no injector is registered for a manifest fault.
	ErrUnknownKind = errors.New("no injector registered for fault kind")
	// ErrBadOptions means the fault options do not fit the injector.
	ErrBadOptions = errors.New("fault options rejected by injector")
)

// Fault is the manifest fault projected for an injector.
type Fault struct {
	// Kind is one of the Kinds constants.
	Kind string
	// Target is the service the fault applies to.
	Target string
	// Options are the injector specific parameters.
	Options map[string]any
	// Trigger is "on_start", "after_30s" or "manual".
	Trigger string
	// BreaksLoop marks a chain link whose removal breaks the fault loop.
	BreaksLoop bool
}

// Environment is what an injector may do to an environment. It is an
// interface, not the Adapter itself, so an injector can be tested without an
// engine and so the core can forbid writes an injector should not perform.
type Environment interface {
	// InstanceID returns the session the fault is activated in.
	InstanceID() string
	// Runtime exposes the adapter for reads and for config patches.
	Runtime() runtime.Adapter
	// SetEnv patches service environment and restarts the service. It is the
	// write primitive of feature_flag and sql_config injectors.
	SetEnv(ctx context.Context, service string, vars map[string]string) error
	// ExecSQL runs a statement against a database service.
	ExecSQL(ctx context.Context, service string, statement string) (runtime.ExecResult, error)
}

// Injector activates one kind of fault.
type Injector interface {
	// Kind returns the fault kind this injector handles.
	Kind() string
	// Activate breaks the environment as declared by the manifest.
	Activate(ctx context.Context, env Environment, fault Fault) error
}

// Registry maps fault kinds to injectors. The core builds one registry per
// process from the available adapters, so the set of injectors is a property
// of the deployment and not of the task.
type Registry struct {
	mu        sync.RWMutex
	injectors map[string]Injector
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{injectors: map[string]Injector{}}
}

// Register adds an injector. Registering the same kind twice replaces the
// previous one: a deployment can override a built-in injector without a
// config flag.
func (r *Registry) Register(injector Injector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.injectors[injector.Kind()] = injector
}

// Get returns the injector for a fault kind.
func (r *Registry) Get(kind string) (Injector, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	injector, ok := r.injectors[kind]
	if !ok {
		return nil, fmt.Errorf("%w %q (registered: %s)", ErrUnknownKind, kind, r.registeredLocked())
	}
	return injector, nil
}

// Kinds returns the registered kinds, sorted.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.injectors))
	for kind := range r.injectors {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func (r *Registry) registeredLocked() string {
	if len(r.injectors) == 0 {
		return "none"
	}
	kinds := make([]string, 0, len(r.injectors))
	for kind := range r.injectors {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return fmt.Sprint(kinds)
}

// StubInjector is the placeholder used until an engine-backed injector exists.
// It exists so the contract is testable before the implementations and so a
// manifest that declares an unimplemented fault fails loudly instead of
// silently starting a healthy environment.
type StubInjector struct {
	KindValue string
	// Called records the faults the stub was asked to activate.
	Called []Fault
}

// NewStub returns a stub for a fault kind.
func NewStub(kind string) *StubInjector { return &StubInjector{KindValue: kind} }

// Kind implements Injector.
func (s *StubInjector) Kind() string { return s.KindValue }

// Activate implements Injector. It never changes the environment: the runtime
// contract guarantees a task either runs its declared fault or does not start.
func (s *StubInjector) Activate(ctx context.Context, env Environment, fault Fault) error {
	s.Called = append(s.Called, fault)
	return fmt.Errorf("%w: %s injector is a stub", ErrBadOptions, fault.Kind)
}

// WithStubs returns a registry holding a stub for every known fault kind.
func WithStubs() *Registry {
	registry := NewRegistry()
	for _, kind := range Kinds() {
		registry.Register(NewStub(kind))
	}
	return registry
}
