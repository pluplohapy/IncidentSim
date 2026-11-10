package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"incidentsim/internal/injector"
	"incidentsim/internal/manifest"
	"incidentsim/internal/runtime"
	"incidentsim/internal/verifier"
)

// Orchestrator runs the task lifecycle: it provisions an environment, breaks
// it as the manifest declares, and grades the participant's fix.
//
// It is the only place where the manifest, the runtime and the injectors meet,
// which keeps the adapters free of task semantics and the manifest free of
// engine details.
type Orchestrator struct {
	Config    Config
	Catalog   *Catalog
	Sessions  *Manager
	Adapters  map[string]runtime.Adapter
	Injectors *injector.Registry
	Verifier  verifier.Verifier

	mu sync.Mutex
	// sequence keeps session ids monotonic inside a process.
	sequence int
}

// New returns an orchestrator with the given engine set. The injector registry
// and the verifier are injected so tests can substitute them.
func New(config Config, catalog *Catalog, adapters map[string]runtime.Adapter, injectors *injector.Registry, grader verifier.Verifier) (*Orchestrator, error) {
	if catalog == nil {
		return nil, errors.New("catalog is required")
	}
	if len(adapters) == 0 {
		return nil, errors.New("at least one runtime adapter is required")
	}
	if _, ok := adapters[config.Engine]; !ok {
		return nil, fmt.Errorf("engine %q has no adapter (have %v)", config.Engine, adapterNames(adapters))
	}
	if injectors == nil {
		injectors = injector.DefaultRegistry()
	}
	if grader == nil {
		grader = verifier.NewManifestVerifier()
	}
	return &Orchestrator{
		Config:    config,
		Catalog:   catalog,
		Sessions:  NewManager(),
		Adapters:  adapters,
		Injectors: injectors,
		Verifier:  grader,
	}, nil
}

func adapterNames(adapters map[string]runtime.Adapter) []string {
	out := make([]string, 0, len(adapters))
	for name := range adapters {
		out = append(out, name)
	}
	return out
}

// Start begins a task: it provisions the environment, activates the declared
// fault and returns the live session.
func (o *Orchestrator) Start(ctx context.Context, taskID string) (*Session, error) {
	task, err := o.Catalog.Load(taskID)
	if err != nil {
		return nil, err
	}
	engine := task.Spec.Runtime.Engine
	adapter, ok := o.Adapters[engine]
	if !ok {
		return nil, fmt.Errorf("task %q needs engine %q, which this deployment does not have (have %v)", taskID, engine, adapterNames(o.Adapters))
	}

	o.mu.Lock()
	o.sequence++
	sequence := o.sequence
	o.mu.Unlock()

	sessionID := fmt.Sprintf("%s-%d-%s", taskID, sequence, time.Now().UTC().Format("150405"))
	session := &Session{
		ID:        sessionID,
		TaskID:    taskID,
		State:     StateStarting,
		StartedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		manifest:  task,
	}
	journal, err := OpenJournal(o.Config.JournalDir(), sessionID)
	if err != nil {
		return nil, err
	}
	session.journal = journal
	o.Sessions.Add(session)

	journal.Record(EventSessionStarted, "session started", map[string]any{"task": taskID, "engine": engine})

	spec := o.runtimeSpec(task)
	instance, err := adapter.Up(ctx, sessionID, spec)
	if err != nil {
		session.SetState(StateFailed)
		journal.Record(EventFaultFailed, "environment failed to start", map[string]any{"error": err.Error()})
		return session, fmt.Errorf("start environment: %w", err)
	}
	session.mu.Lock()
	session.instance = instance
	session.mu.Unlock()
	journal.Record(EventEnvironmentUp, "environment is up", instanceToData(instance))
	session.SetState(StateRunning)

	if err := o.activateFault(ctx, session, adapter); err != nil {
		// A fault that cannot be activated is a broken task, not a broken
		// session: fail loudly instead of handing out a healthy environment.
		session.SetState(StateFailed)
		journal.Record(EventFaultFailed, "fault could not be activated", map[string]any{"error": err.Error()})
		return session, fmt.Errorf("activate fault: %w", err)
	}
	return session, nil
}

// runtimeSpec projects the manifest into what an adapter needs.
func (o *Orchestrator) runtimeSpec(task *manifest.Manifest) runtime.Spec {
	services := make([]runtime.ServiceSpec, 0, len(task.Spec.Services))
	for _, declared := range task.Spec.Services {
		services = append(services, runtime.ServiceSpec{
			Name:        declared.Name,
			Image:       declared.Image,
			Env:         declared.Env,
			Ports:       declared.Ports,
			Command:     declared.Command,
			Healthcheck: projectHealthcheck(declared.Healthcheck),
		})
	}
	faultEnv := map[string]string{}
	if task.Spec.Fault != nil {
		for key, value := range task.Spec.Fault.Options {
			if text, ok := value.(string); ok && key == "env" {
				_ = text // env is a nested map, handled by the injectors
			}
		}
	}
	return runtime.Spec{
		Services:  services,
		Snapshot:  task.Spec.Runtime.Snapshot,
		CPUCount:  task.Spec.Runtime.CPUCount,
		MemoryMiB: task.Spec.Runtime.MemoryMiB,
		FaultEnv:  faultEnv,
	}
}

func projectHealthcheck(check *manifest.Healthcheck) *runtime.HealthcheckSpec {
	if check == nil {
		return nil
	}
	return &runtime.HealthcheckSpec{
		Path:            check.Path,
		Port:            check.Port,
		IntervalSeconds: check.IntervalSeconds,
		TimeoutSeconds:  check.TimeoutSeconds,
	}
}

// activateFault runs every injector the manifest declares.
//
// When no injector is registered for a fault kind, an environment fault
// expressed purely as variables is applied through the runtime adapter. That
// fallback exists because the compose engine can express a config fault
// without a guest agent, and it is recorded in the journal so a report never
// claims an injector ran when it did not.
func (o *Orchestrator) activateFault(ctx context.Context, session *Session, adapter runtime.Adapter) error {
	task := session.manifest
	faults := []manifest.FaultSpec{}
	if task.Spec.Fault != nil {
		faults = append(faults, *task.Spec.Fault)
	}
	faults = append(faults, task.Spec.FaultChain...)

	if len(faults) == 0 {
		return errors.New("manifest declares neither fault nor fault_chain")
	}
	for _, declared := range faults {
		fault := injector.Fault{
			Kind:       declared.Type,
			Target:     declared.Target,
			Options:    declared.Options,
			Trigger:    declared.Trigger,
			BreaksLoop: declared.BreaksLoop,
		}
		environment := &sessionEnv{session: session, adapter: adapter}
		registered, err := o.Injectors.Get(declared.Type)
		if err != nil {
			envPatch, ok := envPatchFromFault(declared)
			if !ok {
				return fmt.Errorf("fault %q on %q: %w", declared.Type, declared.Target, err)
			}
			instance, applyErr := adapter.Apply(ctx, session.ID, envPatch)
			if applyErr != nil {
				return fmt.Errorf("apply fault env: %w", applyErr)
			}
			o.refreshInstance(session, instance)

			session.journal.Record(EventFaultActivated, "fault applied through the runtime adapter", map[string]any{
				"kind":   declared.Type,
				"target": declared.Target,
				"vars":   envPatch.Vars,
				"note":   "no injector registered for this kind",
			})
			continue
		}
		// A database injected before the database is up fails on a race, and
		// the failure looks exactly like a broken task instead of a slow boot.
		if declared.Type == injector.KindSQLConfig {
			if err := o.waitForDatabase(ctx, adapter, session, declared.Target); err != nil {
				return err
			}
		}
		if err := registered.Activate(ctx, environment, fault); err != nil {
			return fmt.Errorf("injector %q: %w", declared.Type, err)
		}
		session.journal.Record(EventFaultActivated, "fault activated by injector", map[string]any{
			"kind":   declared.Type,
			"target": declared.Target,
		})
	}
	return nil
}

// envPatchFromFault extracts the environment patch of a fault that is nothing
// but variables, which is what the runtime-adapter fallback needs.
func envPatchFromFault(fault manifest.FaultSpec) (runtime.EnvPatch, bool) {
	raw, ok := fault.Options["env"]
	if !ok {
		return runtime.EnvPatch{}, false
	}
	vars, ok := raw.(map[string]any)
	if !ok || len(vars) == 0 {
		return runtime.EnvPatch{}, false
	}
	patch := runtime.EnvPatch{Service: fault.Target, Restart: true, Vars: map[string]string{}}
	for key, value := range vars {
		patch.Vars[key] = fmt.Sprint(value)
	}
	return patch, true
}

// Verify grades the current state of a session.
func (o *Orchestrator) Verify(ctx context.Context, sessionID string) (verifier.Report, error) {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return verifier.Report{}, err
	}
	session.SetState(StateVerifying)
	adapter, err := o.adapterFor(session)
	if err != nil {
		return verifier.Report{}, err
	}
	report, err := o.Verifier.Verify(ctx, &verifyEnv{session: session, adapter: adapter}, session.ID, session.TaskID, Checks(session.manifest))
	session.mu.Lock()
	session.LastVerdict = report.Verdict
	session.mu.Unlock()
	session.SetState(StateRunning)
	session.journal.Record(EventVerifyRun, report.String(), map[string]any{
		"verdict":     report.Verdict,
		"correctness": report.Correctness.Verdict,
		"quality":     report.Quality.Verdict,
		"replay":      report.Replay,
	})
	return report, err
}

// Reset restores the baseline environment of a session.
func (o *Orchestrator) Reset(ctx context.Context, sessionID string) error {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return err
	}
	adapter, err := o.adapterFor(session)
	if err != nil {
		return err
	}
	instance, err := adapter.Reset(ctx, session.ID)
	if err != nil {
		return fmt.Errorf("reset environment: %w", err)
	}
	o.refreshInstance(session, instance)
	session.mu.Lock()
	session.AppliedFixes = nil
	session.FaultArtifacts = nil
	session.LastVerdict = ""
	session.mu.Unlock()
	session.journal.Record(EventReset, "environment restored to baseline", nil)

	// A reset means "back to the incident", so the declared fault has to be
	// activated again: without this the participant would be handed a healthy
	// environment after a reset and would have nothing to diagnose.
	if err := o.activateFault(ctx, session, adapter); err != nil {
		return fmt.Errorf("re-activate fault after reset: %w", err)
	}
	return nil
}

// Apply pushes a participant fix into the environment.
func (o *Orchestrator) Apply(ctx context.Context, sessionID string, patch runtime.EnvPatch) error {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return err
	}
	adapter, err := o.adapterFor(session)
	if err != nil {
		return err
	}
	instance, err := adapter.Apply(ctx, session.ID, patch)
	if err != nil {
		session.journal.Record(EventFixFailed, "fix could not be applied", map[string]any{
			"service": patch.Service,
			"vars":    patch.Vars,
			"error":   err.Error(),
		})
		return fmt.Errorf("apply fix: %w", err)
	}
	o.refreshInstance(session, instance)
	session.mu.Lock()
	session.AppliedFixes = append(session.AppliedFixes, patch)
	session.mu.Unlock()
	session.journal.Record(EventFixApplied, "participant applied a change", map[string]any{
		"service": patch.Service,
		"vars":    patch.Vars,
		"restart": patch.Restart,
	})
	return nil
}

// ExecSQL runs a statement against a database service of a session. It is the
// participant's write path for a db-level task: `incidentsim apply --sql` ends
// up here.
func (o *Orchestrator) ExecSQL(ctx context.Context, sessionID, service, statement string) (runtime.ExecResult, error) {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	adapter, err := o.adapterFor(session)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	result, err := adapter.Exec(ctx, session.ID, runtime.ExecRequest{
		Service: service,
		Command: sqlCommand(session.manifest, service, statement),
		Timeout: 30 * time.Second,
	})
	if err == nil && result.ExitCode != 0 {
		err = fmt.Errorf("statement exited %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	if err != nil {
		session.journal.Record(EventFixFailed, "statement could not be applied", map[string]any{
			"service":   service,
			"statement": statement,
			"error":     err.Error(),
		})
		return result, err
	}
	session.mu.Lock()
	session.AppliedFixes = append(session.AppliedFixes, runtime.EnvPatch{
		Service: service,
		Command: sqlCommand(session.manifest, service, statement),
	})
	session.mu.Unlock()
	session.journal.Record(EventFixApplied, "participant ran a statement", map[string]any{
		"service":   service,
		"statement": statement,
	})
	return result, nil
}

// Logs returns the filtered logs of a service of a session.
func (o *Orchestrator) Logs(ctx context.Context, sessionID, service string, opts runtime.LogOptions) ([]runtime.LogLine, error) {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return nil, err
	}
	adapter, err := o.adapterFor(session)
	if err != nil {
		return nil, err
	}
	if opts.Service == "" {
		opts.Service = service
	}
	return adapter.Logs(ctx, session.ID, opts)
}

// Metrics returns the latest metrics of a service of a session.
func (o *Orchestrator) Metrics(ctx context.Context, sessionID, service string) ([]runtime.MetricSample, error) {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return nil, err
	}
	adapter, err := o.adapterFor(session)
	if err != nil {
		return nil, err
	}
	return adapter.Metrics(ctx, session.ID, service)
}

// Hint hands out the next hint of a task and charges the session for it, so
// the postmortem can tell how much help was needed.
func (o *Orchestrator) Hint(sessionID string, level int) (manifest.Hint, bool, error) {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return manifest.Hint{}, false, err
	}
	if level <= 0 {
		session.mu.Lock()
		session.HintsUsed++
		next := session.HintsUsed
		session.mu.Unlock()
		level = next
	}
	for _, hint := range session.manifest.Spec.Hints {
		if hint.Level == level {
			session.journal.Record(EventHintRequested, "hint handed out", map[string]any{"level": level})
			return hint, true, nil
		}
	}
	if level > len(session.manifest.Spec.Hints) {
		return manifest.Hint{Level: level, Text: "Подсказки закончились: смотрите симптомы и метрики."}, false, nil
	}
	return manifest.Hint{Level: level, Text: "Подсказки закончились: смотрите симптомы и метрики."}, false, nil
}

// Stop tears the environment of a session down and keeps the journal.
func (o *Orchestrator) Stop(ctx context.Context, sessionID string) error {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return err
	}
	adapter, err := o.adapterFor(session)
	if err != nil {
		return err
	}
	downErr := adapter.Down(ctx, session.ID)
	session.journal.Record(EventTeardown, "environment torn down", nil)
	session.SetState(StateTornDown)
	session.journal.Close()
	o.Sessions.Remove(session.ID)
	return downErr
}

// waitForDatabase polls a trivial statement until the database answers.
//
// The official postgres image takes a few seconds to initialise, and a sql_config
// fault that runs before that fails with a connection error that looks like a
// broken task. Waiting here keeps the failure mode honest.
func (o *Orchestrator) waitForDatabase(ctx context.Context, adapter runtime.Adapter, session *Session, service string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		result, err := adapter.Exec(ctx, session.ID, runtime.ExecRequest{
			Service: service,
			Command: sqlCommand(session.manifest, service, "SELECT 1"),
			Timeout: 5 * time.Second,
		})
		if err == nil && result.ExitCode == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("database %s did not come up: %w", service, err)
			}
			return fmt.Errorf("database %s did not come up: %s", service, strings.TrimSpace(result.Stderr))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// refreshInstance replaces the endpoints the core knows about. A recreate can
// move a published port, and a verifier that probes a stale endpoint would
// report a broken environment instead of the participant's actual fix.
func (o *Orchestrator) refreshInstance(session *Session, instance *runtime.Instance) {
	if instance == nil {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	session.instance = instance
}

func (o *Orchestrator) adapterFor(session *Session) (runtime.Adapter, error) {
	engine := ""
	session.mu.Lock()
	if session.instance != nil {
		engine = session.instance.Engine
	}
	session.mu.Unlock()
	if engine == "" {
		engine = session.manifest.Spec.Runtime.Engine
	}
	adapter, ok := o.Adapters[engine]
	if !ok {
		return nil, fmt.Errorf("engine %q has no adapter", engine)
	}
	return adapter, nil
}
