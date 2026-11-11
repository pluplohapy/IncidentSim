package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"incidentsim/internal/injector"
	"incidentsim/internal/manifest"
	"incidentsim/internal/runtime"
	"incidentsim/internal/verifier"
)

// ErrOperationInProgress is returned when a session already has an
// apply/verify/reset/stop in progress and a second concurrent operation
// arrives. The HTTP layer turns it into a 409 conflict.
var ErrOperationInProgress = errors.New("operation in progress")

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
	// Audit records what happened in a session for an external system. It is
	// optional and off the main path: a missing or broken audit backend is a
	// warning, never a failed session.
	Audit AuditRecorder

	// Builder compiles a code-level workspace into a deployable artifact. It
	// is set by the deployment (cmd/incidentsim serve) and is required for
	// code-level tasks.
	Builder runtime.Builder

	mu sync.Mutex
	// sequence keeps session ids monotonic inside a process.
	sequence int
	// ops tracks per-session operations that are in progress, so concurrent
	// apply/verify/reset/stop on the same session serialize with a 409
	// instead of interleaving.
	ops map[string]bool
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
		ops:       map[string]bool{},
	}, nil
}

func adapterNames(adapters map[string]runtime.Adapter) []string {
	out := make([]string, 0, len(adapters))
	for name := range adapters {
		out = append(out, name)
	}
	return out
}

// beginOp marks a session as having an operation in progress. A second
// caller for the same session gets ErrOperationInProgress, which the HTTP
// layer renders as a 409 conflict.
func (o *Orchestrator) beginOp(sessionID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ops[sessionID] {
		return ErrOperationInProgress
	}
	o.ops[sessionID] = true
	return nil
}

// endOp releases the in-progress mark for a session.
func (o *Orchestrator) endOp(sessionID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.ops, sessionID)
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

	// Any failure after the session is registered must clean up: the workspace,
	// the environment (compose project / Firecracker VM), the session image and
	// the session entry itself, so a failed Start leaves nothing behind.
	cleanup := func() {
		_ = adapter.Down(ctx, sessionID)
		_ = o.removeSessionImage(ctx, sessionID)
		if session.WorkspaceDir != "" {
			_ = os.RemoveAll(session.WorkspaceDir)
		}
		o.Sessions.Remove(sessionID)
	}

	// For code-level tasks, copy the broken scenario workspace into
	// <StateDir>/sessions/<id>/workspace/ so the participant can edit it.
	if err := o.setupWorkspace(session); err != nil {
		cleanup()
		journal.Record(EventFaultFailed, "workspace setup failed", map[string]any{"error": err.Error()})
		return nil, fmt.Errorf("setup workspace: %w", err)
	}

	spec := o.runtimeSpec(task)
	spec.WorkspaceDir = session.WorkspaceDir
	// Only a code-level task ships a buildable workspace: its scenario has a
	// Dockerfile, and rebuilding the image from the broken code is what makes
	// the initial state the declared one. A config-, db- or composite-level
	// task has a workspace of configuration files only — building an image
	// from it would fail, and none is needed: the adapter writes the config
	// into the compose project and recreates the service.
	if session.WorkspaceDir != "" && task.Metadata.Level == manifest.LevelCode {
		imageTag, err := o.buildWorkspaceImage(ctx, session)
		if err != nil {
			cleanup()
			journal.Record(EventFaultFailed, "workspace image build failed", map[string]any{"error": err.Error()})
			return nil, fmt.Errorf("build workspace image: %w", err)
		}
		for i := range spec.Services {
			if spec.Services[i].Name == "app" {
				spec.Services[i].Image = imageTag
				break
			}
		}
	}

	instance, err := adapter.Up(ctx, sessionID, spec)
	if err != nil {
		cleanup()
		journal.Record(EventFaultFailed, "environment failed to start", map[string]any{"error": err.Error()})
		return nil, fmt.Errorf("start environment: %w", err)
	}
	session.mu.Lock()
	session.instance = instance
	session.mu.Unlock()
	journal.Record(EventEnvironmentUp, "environment is up", instanceToData(instance))
	session.SetState(StateRunning)

	if err := o.activateFault(ctx, session, adapter); err != nil {
		// A fault that cannot be activated is a broken task, not a broken
		// session: fail loudly instead of handing out a healthy environment.
		cleanup()
		session.SetState(StateFailed)
		journal.Record(EventFaultFailed, "fault could not be activated", map[string]any{"error": err.Error()})
		return nil, fmt.Errorf("activate fault: %w", err)
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
		if envVal, ok := task.Spec.Fault.Options["env"]; ok {
			if envMap, ok := envVal.(map[string]any); ok {
				for key, value := range envMap {
					faultEnv[key] = fmt.Sprint(value)
				}
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
		// Code-level tasks carry the defect in the scenario workspace;
		// nothing to inject.
		return nil
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
	if err := o.beginOp(sessionID); err != nil {
		return verifier.Report{}, err
	}
	defer o.endOp(sessionID)

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
	if err := o.beginOp(sessionID); err != nil {
		return err
	}
	defer o.endOp(sessionID)

	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return err
	}
	adapter, err := o.adapterFor(session)
	if err != nil {
		return err
	}
	// Reset the participant workspace: remove any changes and copy the
	// broken scenario workspace again so the reset returns to the incident.
	if session.WorkspaceDir != "" {
		if err := os.RemoveAll(session.WorkspaceDir); err != nil {
			return fmt.Errorf("remove workspace: %w", err)
		}
		session.mu.Lock()
		session.WorkspaceDir = ""
		session.mu.Unlock()
		if err := o.setupWorkspace(session); err != nil {
			return fmt.Errorf("re-setup workspace: %w", err)
		}
	}

	// Only a code-level fix needs a rebuilt image. Compose Reset restores the
	// baseline, which already carries the code-level image tag (substituted at
	// Start). Config-level and db-level tasks don't rebuild images.
	if session.WorkspaceDir != "" && session.manifest.Metadata.Level == manifest.LevelCode {
		imageTag, err := o.buildWorkspaceImage(ctx, session)
		if err != nil {
			return fmt.Errorf("build workspace image: %w", err)
		}
		// A new image changes nothing while the old container keeps running:
		// the service has to be recreated from it, which is what the restart
		// asks the adapter to do. Without it the reset of a code-level task
		// would hand out the participant's fixed image as the incident.
		if _, err := adapter.Apply(ctx, session.ID, runtime.EnvPatch{Service: "app", Image: imageTag, Restart: true}); err != nil {
			return fmt.Errorf("apply image tag: %w", err)
		}
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
//
// Two paths:
//
//   - patch.Files set: the internal --file/--content API. Files are written
//     into the session workspace, then the workspace is deployed exactly as
//     the no-files path below.
//   - patch.Files empty: `incidentsim apply <session>` picks up the current
//     state of the session workspace, validates it, builds it (code level) or
//     deploys it (config/db level), and restarts the service.
//
// A build failure returns the compiler output to the participant and leaves
// the running environment untouched.
func (o *Orchestrator) Apply(ctx context.Context, sessionID string, patch runtime.EnvPatch) error {
	if err := o.beginOp(sessionID); err != nil {
		return err
	}
	defer o.endOp(sessionID)

	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return err
	}
	adapter, err := o.adapterFor(session)
	if err != nil {
		return err
	}

	// Write files carried by the patch into the session workspace first.
	workspaceMode := false
	if len(patch.Files) > 0 {
		if err := o.writePatchFiles(session, patch); err != nil {
			return err
		}
		workspaceMode = true
	}

	// A session without a workspace (e.g. a config-level task whose fix is
	// purely env vars) takes the direct vars/command path. Anything else
	// applies from the current workspace state.
	if len(patch.Vars) == 0 && len(patch.Command) == 0 && session.WorkspaceDir != "" {
		workspaceMode = true
	}

	if workspaceMode {
		return o.applyFromWorkspace(ctx, session, adapter, patch)
	}

	// Direct vars/command path: no workspace involved.
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

// writePatchFiles writes the files carried by a patch into the session
// workspace, rejecting path traversal and non-editable files before anything
// is written.
func (o *Orchestrator) writePatchFiles(session *Session, patch runtime.EnvPatch) error {
	for relPath := range patch.Files {
		if err := validateFilePath(relPath); err != nil {
			return fmt.Errorf("invalid file path %q: %w", relPath, err)
		}
	}
	allowed := make(map[string]bool)
	for _, e := range session.manifest.Spec.Editable {
		allowed[e] = true
	}
	for relPath := range patch.Files {
		if !allowed[relPath] {
			return fmt.Errorf("file %q is not in editable list %v", relPath, session.manifest.Spec.Editable)
		}
	}
	for relPath, content := range patch.Files {
		target := filepath.Join(session.WorkspaceDir, relPath)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create dir for %s: %w", relPath, err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", relPath, err)
		}
	}
	return nil
}

// applyFromWorkspace validates, builds/deploys and restarts the service from
// the current state of the session workspace.
func (o *Orchestrator) applyFromWorkspace(ctx context.Context, session *Session, adapter runtime.Adapter, patch runtime.EnvPatch) error {
	// Validate workspace integrity against the scenario: a changed, added or
	// deleted file outside spec.editable is rejected and the workspace is left
	// untouched.
	if err := o.validateEditableFiles(session); err != nil {
		return fmt.Errorf("editable validation: %w", err)
	}

	// Journal the unified diff of the workspace against the scenario (not the
	// full contents).
	if err := o.journalWorkspaceDiff(session); err != nil {
		return fmt.Errorf("workspace diff: %w", err)
	}

	session.journal.Record(EventApplyStarted, "apply started", map[string]any{
		"service": patch.Service,
		"level":   session.manifest.Metadata.Level,
	})

	switch session.manifest.Metadata.Level {
	case manifest.LevelCode:
		if o.Builder == nil {
			return fmt.Errorf("no builder configured for code-level task %s", session.TaskID)
		}
		artifact, err := o.Builder.Build(ctx, session.WorkspaceDir)
		if err != nil {
			// Build failure: return the compiler output, do not touch the
			// running environment.
			session.journal.Record(EventBuildFailed, "build failed", map[string]any{"error": err.Error()})
			return fmt.Errorf("build workspace: %w", err)
		}
		session.journal.Record(EventBuildOK, "build ok", map[string]any{
			"service": artifact.Service,
			"digest":  artifact.Digest,
		})
		instance, err := adapter.Deploy(ctx, session.ID, *artifact, true)
		if err != nil {
			session.journal.Record(EventFixFailed, "deploy failed", map[string]any{"error": err.Error()})
			return fmt.Errorf("deploy: %w", err)
		}
		o.refreshInstance(session, instance)
		if err := o.waitForHealth(ctx, session, adapter); err != nil {
			return fmt.Errorf("service not healthy after deploy: %w", err)
		}
	case manifest.LevelConfig:
		// Config files are deployed as-is; there is no build.
		artifact, err := o.configArtifact(session)
		if err != nil {
			return fmt.Errorf("config artifact: %w", err)
		}
		instance, err := adapter.Deploy(ctx, session.ID, artifact, true)
		if err != nil {
			session.journal.Record(EventFixFailed, "deploy failed", map[string]any{"error": err.Error()})
			return fmt.Errorf("deploy: %w", err)
		}
		o.refreshInstance(session, instance)
		if err := o.waitForHealth(ctx, session, adapter); err != nil {
			return fmt.Errorf("service not healthy after deploy: %w", err)
		}
	case manifest.LevelDB:
		if len(patch.Command) == 0 {
			return fmt.Errorf("db-level apply requires a SQL statement")
		}
		if _, err := adapter.Exec(ctx, session.ID, runtime.ExecRequest{
			Service: patch.Service,
			Command: patch.Command,
		}); err != nil {
			session.journal.Record(EventFixFailed, "statement failed", map[string]any{"error": err.Error()})
			return fmt.Errorf("sql: %w", err)
		}
	default:
		return fmt.Errorf("unsupported level %q for apply", session.manifest.Metadata.Level)
	}

	session.mu.Lock()
	session.AppliedFixes = append(session.AppliedFixes, patch)
	session.mu.Unlock()
	session.journal.Record(EventServiceRecreated, "service recreated", map[string]any{
		"service": patch.Service,
	})
	session.journal.Record(EventFixApplied, "participant applied a change", map[string]any{
		"service": patch.Service,
		"vars":    patch.Vars,
		"restart": patch.Restart,
	})
	return nil
}

// configArtifact builds a Kind=files artifact from the session workspace, so
// config-level tasks deploy the files the service actually loads.
func (o *Orchestrator) configArtifact(session *Session) (runtime.Artifact, error) {
	artifact := runtime.Artifact{Service: "app", Kind: "files", Files: map[string][]byte{}}
	err := filepath.WalkDir(session.WorkspaceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(session.WorkspaceDir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		artifact.Files[rel] = data
		return nil
	})
	return artifact, err
}

// journalWorkspaceDiff records a unified diff of the session workspace against
// the scenario workspace, so the journal shows what changed rather than the
// full contents of every file.
func (o *Orchestrator) journalWorkspaceDiff(session *Session) error {
	diff, err := o.workspaceDiffText(session)
	if err != nil {
		return err
	}
	if diff == "" {
		return nil
	}
	session.journal.Record(EventApplyStarted, "workspace diff", map[string]any{
		"diff": diff,
	})
	return nil
}

// waitForHealth waits until every service of the session answers its
// healthcheck, then refreshes the instance so endpoints are current.
func (o *Orchestrator) waitForHealth(ctx context.Context, session *Session, adapter runtime.Adapter) error {
	session.mu.Lock()
	instance := session.instance
	session.mu.Unlock()
	if instance == nil {
		return fmt.Errorf("no instance for session %s", session.ID)
	}
	return runtime.WaitForHealth(ctx, instance, runtime.DefaultResetTimeout)
}

// Diff returns the unified diff of the session workspace against the scenario
// workspace, so `incidentsim diff <session>` shows what the participant
// changed without dumping full file contents.
func (o *Orchestrator) Diff(sessionID string) (string, error) {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return "", err
	}
	return o.workspaceDiffText(session)
}

// workspaceDiffText builds the unified diff of dst (session workspace) against
// src (scenario workspace). It does not journal; callers that want an audit
// trail use journalWorkspaceDiff instead.
func (o *Orchestrator) workspaceDiffText(session *Session) (string, error) {
	src := o.scenarioWorkspaceDir(session)
	dst := session.WorkspaceDir
	if src == "" || dst == "" {
		return "", nil
	}
	var diff strings.Builder
	_ = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		scenarioData, serr := os.ReadFile(path)
		dstData, derr := os.ReadFile(filepath.Join(dst, rel))
		switch {
		case serr != nil:
			fmt.Fprintf(&diff, "--- %s\n+++ /dev/null\n@@\n- %s\n", rel, string(scenarioData))
		case derr != nil:
			fmt.Fprintf(&diff, "--- /dev/null\n+++ %s\n@@\n+ %s\n", rel, string(scenarioData))
		default:
			if bytes.Equal(scenarioData, dstData) {
				return nil
			}
			fmt.Fprintf(&diff, "--- %s\n+++ %s\n@@\n- %s\n+ %s\n", rel, rel, string(scenarioData), string(dstData))
		}
		return nil
	})
	filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		if _, serr := os.ReadFile(filepath.Join(src, rel)); serr != nil {
			data, derr := os.ReadFile(path)
			if derr != nil {
				return derr
			}
			fmt.Fprintf(&diff, "--- /dev/null\n+++ %s\n@@\n+ %s\n", rel, string(data))
		}
		return nil
	})
	return diff.String(), nil
}

// Exec runs an arbitrary command inside a service of a session.
//
// It is the participant's debug path: `incidentsim exec --service app -- sh -c
// 'cat /etc/app/app.conf'` ends up here, and so does every diagnostic a real
// incident needs — ps, ss, tcpdump, a manual psql session. The environment is
// per session and ephemeral, so the debug is as safe as the task itself: a
// change written inside a container disappears on the next recreate, and only
// Apply can change what a verification grades.
func (o *Orchestrator) Exec(ctx context.Context, sessionID string, req runtime.ExecRequest) (runtime.ExecResult, error) {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	adapter, err := o.adapterFor(session)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	if req.Timeout <= 0 {
		req.Timeout = 30 * time.Second
	}
	result, err := adapter.Exec(ctx, session.ID, req)
	if err == nil && result.ExitCode != 0 {
		err = fmt.Errorf("command exited %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	if err != nil {
		session.journal.Record(EventExecFailed, "command could not be executed", map[string]any{
			"service": req.Service,
			"command": req.Command,
			"error":   err.Error(),
		})
		return result, err
	}
	session.journal.Record(EventExecRun, "participant ran a command in the environment", map[string]any{
		"service": req.Service,
		"command": req.Command,
	})
	return result, nil
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

// Env returns the effective environment of a service of a session.
//
// It is how a participant diagnoses a config-level incident: the fault is a
// variable, and the variable is visible through the same adapter path the
// policy checks use.
func (o *Orchestrator) Env(ctx context.Context, sessionID, service string) (map[string]string, error) {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return nil, err
	}
	adapter, err := o.adapterFor(session)
	if err != nil {
		return nil, err
	}
	return (&verifyEnv{session: session, adapter: adapter}).Env(ctx, service)
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
	if err := o.beginOp(sessionID); err != nil {
		return err
	}
	defer o.endOp(sessionID)

	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return err
	}
	adapter, err := o.adapterFor(session)
	if err != nil {
		return err
	}
	downErr := adapter.Down(ctx, session.ID)
	// The participant workspace lives under the state dir; stop removes it so
	// no scratch files survive the session.
	if session.WorkspaceDir != "" {
		if rerr := os.RemoveAll(session.WorkspaceDir); rerr != nil && downErr == nil {
			downErr = fmt.Errorf("remove workspace: %w", rerr)
		}
	}
	// A code-level task baked a session image; drop it on teardown.
	if session.manifest != nil && session.manifest.Metadata.Level == manifest.LevelCode {
		if rerr := o.removeSessionImage(ctx, sessionID); rerr != nil && downErr == nil {
			downErr = fmt.Errorf("remove session image: %w", rerr)
		}
	}
	session.journal.Record(EventTeardown, "environment torn down", nil)
	session.SetState(StateTornDown)
	o.audit(session, EventSessionFinished)
	session.journal.Close()
	o.Sessions.Remove(session.ID)
	return downErr
}

// GiveUp records that the participant asked for the answer. After a give-up
// the private view (root cause, reference, distractors) may be revealed, same
// as after a passing verification.
func (o *Orchestrator) GiveUp(ctx context.Context, sessionID string) error {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.GaveUp = true
	session.mu.Unlock()
	session.journal.Record(EventGaveUp, "participant gave up and asked for the answer", nil)
	return nil
}

// canReveal reports whether the private view of a task may be shown for a
// session: after a passing verification or after an explicit give-up.
func (o *Orchestrator) canReveal(sessionID string) (bool, error) {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		return false, err
	}
	session.mu.Lock()
	verdict := session.LastVerdict
	gaveUp := session.GaveUp
	session.mu.Unlock()
	return verdict == verifier.VerdictPass || gaveUp, nil
}

// removeSessionImage drops the docker image baked from a code-level session
// workspace. It is best-effort: a missing image or a missing docker binary
// must not fail teardown.
func (o *Orchestrator) removeSessionImage(ctx context.Context, sessionID string) error {
	tag := fmt.Sprintf("incidentsim/workload:session-%s", sessionID)
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, "docker", "rmi", "-f", tag)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker rmi %s: %w: %s", tag, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// audit records a session event for the configured backend. It never fails the
// operation it is called from, because audit is an obligation and not a gate.
func (o *Orchestrator) audit(session *Session, eventType string) {
	if o.Audit == nil {
		return
	}
	record := SessionRecord{
		SessionID: session.ID,
		TaskID:    session.TaskID,
		StartedAt: session.StartedAt,
		EndedAt:   time.Now().UTC(),
	}
	if err := o.Audit.Record(context.Background(), record); err != nil {
		session.journal.Record(eventType, "audit could not record the session", map[string]any{"error": err.Error()})
		return
	}
	session.journal.Record(eventType, "session recorded for audit", map[string]any{"backend": o.Audit.Name()})
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

// workspaceDir returns the per-session workspace directory:
// <StateDir>/sessions/<id>/workspace/.
func (o *Orchestrator) workspaceDir(sessionID string) (string, error) {
	return filepath.Join(o.Config.StateDir(), "sessions", sessionID, "workspace"), nil
}

// scenarioWorkspaceDir returns the source scenario workspace of a task.
// It uses spec.workspace from the manifest when declared, falling back to
// scenarios/<task>/workspace/ for backward compatibility.
func (o *Orchestrator) scenarioWorkspaceDir(session *Session) string {
	if session.manifest.Spec.Workspace != "" {
		return filepath.Join(o.Config.ProjectDir(), session.manifest.Spec.Workspace)
	}
	return filepath.Join(o.Config.ProjectDir(), "scenarios", session.TaskID, "workspace")
}

// setupWorkspace copies the scenario workspace into the session workspace.
func (o *Orchestrator) setupWorkspace(session *Session) error {
	src := o.scenarioWorkspaceDir(session)
	dst, err := o.workspaceDir(session.ID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return fmt.Errorf("copy workspace: %w", err)
	}
	session.mu.Lock()
	session.WorkspaceDir = dst
	session.mu.Unlock()
	return nil
}

// validateEditableFiles checks that only files listed in spec.editable were
// modified in the session workspace compared to the scenario workspace.
// Files are compared by SHA-256. A file added or deleted outside the
// editable list is an error, as is a modified one. The check runs on both
// sides: the session workspace (changes and additions) and the scenario
// workspace (deletions the dst-side walk cannot see).
func (o *Orchestrator) validateEditableFiles(session *Session) error {
	editable := session.manifest.Spec.Editable
	if len(editable) == 0 {
		return nil
	}
	src := o.scenarioWorkspaceDir(session)
	dst := session.WorkspaceDir
	if src == "" || dst == "" {
		return nil
	}
	allowed := make(map[string]bool)
	for _, e := range editable {
		allowed[e] = true
	}
	var modified []string
	// Session workspace side: changed or added files outside editable.
	err := filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		if allowed[rel] {
			return nil
		}
		scenarioPath := filepath.Join(src, rel)
		scenarioData, serr := os.ReadFile(scenarioPath)
		if serr == nil {
			dstData, derr := os.ReadFile(path)
			if derr == nil && hashEqual(scenarioData, dstData) {
				return nil // unchanged from scenario, not a real modification
			}
		}
		modified = append(modified, rel)
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan workspace: %w", err)
	}
	// Scenario side: files present in the scenario but missing from the
	// session workspace are deletions outside the editable list.
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
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
		if allowed[rel] {
			return nil
		}
		if _, err := os.Stat(filepath.Join(dst, rel)); os.IsNotExist(err) {
			modified = append(modified, rel+" (deleted)")
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan scenario: %w", err)
	}
	if len(modified) > 0 {
		return fmt.Errorf("non-editable files changed: %s (only %v are editable)",
			strings.Join(modified, ", "), editable)
	}
	return nil
}

// hashEqual compares two byte slices by SHA-256 in constant time.
func hashEqual(a, b []byte) bool {
	return sha256.Sum256(a) == sha256.Sum256(b)
}

// buildWorkspaceImage builds a Docker image from the session workspace
// and returns the image tag.
func (o *Orchestrator) buildWorkspaceImage(ctx context.Context, session *Session) (string, error) {
	session.mu.Lock()
	dst := session.WorkspaceDir
	session.mu.Unlock()
	if dst == "" {
		return "", fmt.Errorf("no workspace for session %s", session.ID)
	}
	tag := fmt.Sprintf("incidentsim/workload:session-%s", session.ID)
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", tag, ".")
	cmd.Dir = dst
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		session.journal.Record(EventBuildFailed, "docker build failed", map[string]any{
			"service": "app",
			"tag":     tag,
			"stderr":  stderr.String(),
		})
		return "", fmt.Errorf("docker build: %w: %s", err, stderr.String())
	}
	session.journal.Record(EventBuildOK, "docker image built", map[string]any{
		"service": "app",
		"tag":     tag,
	})
	return tag, nil
}

// validateFilePath rejects absolute paths and any ..-segment so that a
// patch file can never be written outside the session workspace.
func validateFilePath(relPath string) error {
	if filepath.IsAbs(relPath) {
		return fmt.Errorf("absolute paths are not allowed")
	}
	for _, part := range strings.Split(relPath, string(filepath.Separator)) {
		if part == ".." {
			return fmt.Errorf("path contains a parent-directory segment")
		}
	}
	if cleaned := filepath.Clean(relPath); cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes the workspace")
	}
	return nil
}
