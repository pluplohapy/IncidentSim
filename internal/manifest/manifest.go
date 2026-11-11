// Package manifest describes the fixed schema of an IncidentSim task.
//
// The schema is deliberately fixed: manifests are data, not code. Adding a new
// task never requires changing the core, and adding a new concept requires an
// explicit schema change that is reviewed like any other contract.
package manifest

// APIVersion is the only supported manifest API version.
const APIVersion = "incidentsim/v1"

// Kind is the only supported manifest kind.
const Kind = "Incident"

// Levels describe which layer of the system the root cause lives on.
const (
	LevelConfig    = "config"
	LevelCode      = "code"
	LevelDB        = "db"
	LevelComposite = "composite"
)

// Gates are the two verification levels. Correctness answers "does the system
// work now", quality answers "is the fix appropriate for production".
const (
	GateCorrectness = "correctness"
	GateQuality     = "quality"
)

// Fault kinds understood by the injector registry.
const (
	FaultFeatureFlag = "feature_flag"
	FaultSQLConfig   = "sql_config"
	FaultKill        = "kill"
	FaultNetwork     = "network"
	FaultStress      = "stress"
)

// Check kinds understood by the verifier.
const (
	CheckHTTP   = "http"
	CheckMetric = "metric"
	CheckSQL    = "sql"
	CheckLog    = "log"
	CheckPolicy = "policy"
	CheckExec   = "exec"
	CheckReplay = "replay"
	CheckUnit   = "unit"
)

// Trigger values for fault activation.
const (
	TriggerOnStart = "on_start"
	TriggerManual  = "manual"
)

// Manifest is the root of a task definition.
type Manifest struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion"`
	Kind       string   `yaml:"kind" json:"kind"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Spec     `yaml:"spec" json:"spec"`

	// raw is the document this manifest was parsed from. Validation runs
	// against it so that unknown fields are rejected instead of being silently
	// dropped by the typed unmarshal.
	raw []byte
}

// Metadata identifies the task in the catalog.
type Metadata struct {
	ID               string   `yaml:"id" json:"id"`
	Title            string   `yaml:"title" json:"title"`
	Level            string   `yaml:"level" json:"level"`
	Tags             []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	EstimatedMinutes int      `yaml:"estimated_minutes,omitempty" json:"estimated_minutes,omitempty"`
}

// Spec holds the environment, the fault and everything needed to grade a run.
type Spec struct {
	Runtime           RuntimeSpec       `yaml:"runtime" json:"runtime"`
	Services          []ServiceSpec     `yaml:"services" json:"services"`
	Symptoms          []Symptom         `yaml:"symptoms,omitempty" json:"symptoms,omitempty"`
	Hints             []Hint            `yaml:"hints,omitempty" json:"hints,omitempty"`
	Fault             *FaultSpec        `yaml:"fault,omitempty" json:"fault,omitempty"`
	FaultChain        []FaultSpec       `yaml:"fault_chain,omitempty" json:"fault_chain,omitempty"`
	Verify            VerifySpec        `yaml:"verify" json:"verify"`
	RootCause         RootCause         `yaml:"root_cause" json:"root_cause"`
	ReferenceSolution ReferenceSolution `yaml:"reference_solution" json:"reference_solution"`
	Replay            *ReplaySpec       `yaml:"replay,omitempty" json:"replay,omitempty"`
	Distractors       []Distractor      `yaml:"distractors,omitempty" json:"distractors,omitempty"`
	Theory            string            `yaml:"theory,omitempty" json:"theory,omitempty"`
	Editable          []string          `yaml:"editable,omitempty" json:"editable,omitempty"`
	Workspace         string            `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	Reference         string            `yaml:"reference,omitempty" json:"reference,omitempty"`
}

// RuntimeSpec selects the Docker Compose runtime and resource limits.
type RuntimeSpec struct {
	Engine    string `yaml:"engine" json:"engine"` // compose
	Snapshot  string `yaml:"snapshot,omitempty" json:"snapshot,omitempty"`
	CPUCount  int    `yaml:"cpu_count,omitempty" json:"cpu_count,omitempty"`
	MemoryMiB int    `yaml:"memory_mib,omitempty" json:"memory_mib,omitempty"`
}

// ServiceSpec is one process of the task environment.
type ServiceSpec struct {
	Name        string            `yaml:"name" json:"name"`
	Image       string            `yaml:"image" json:"image"`
	Env         map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Ports       []int             `yaml:"ports,omitempty" json:"ports,omitempty"`
	Command     []string          `yaml:"command,omitempty" json:"command,omitempty"`
	// Entrypoint overrides the image ENTRYPOINT. A task image may declare an
	// ENTRYPOINT the manifest must replace (for example the workload image
	// defaults to the legacy service while a dep-stack service is the
	// depstub): compose `command` alone only appends arguments.
	Entrypoint []string          `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty"`
	Healthcheck *Healthcheck      `yaml:"healthcheck,omitempty" json:"healthcheck,omitempty"`
}

// Healthcheck describes how the adapter decides a service is usable.
type Healthcheck struct {
	Path            string `yaml:"path" json:"path"`
	Port            int    `yaml:"port" json:"port"`
	IntervalSeconds int    `yaml:"interval_seconds,omitempty" json:"interval_seconds,omitempty"`
	TimeoutSeconds  int    `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
}

// Symptom is what the participant observes before diagnosing.
type Symptom struct {
	ID          string `yaml:"id" json:"id"`
	Observation string `yaml:"observation" json:"observation"`
}

// Hint is a levelled nudge handed out by `incidentsim hint`.
type Hint struct {
	Level int    `yaml:"level" json:"level"`
	Text  string `yaml:"text" json:"text"`
}

// FaultSpec is one fault injected into the environment by the injector.
type FaultSpec struct {
	Type       string         `yaml:"type" json:"type"`
	Target     string         `yaml:"target" json:"target"`
	Trigger    string         `yaml:"trigger,omitempty" json:"trigger,omitempty"`
	Options    map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
	BreaksLoop bool           `yaml:"breaks_loop,omitempty" json:"breaks_loop,omitempty"`
}

// VerifySpec groups checks by gate. A run passes only when every correctness
// check passes and every quality check passes.
type VerifySpec struct {
	Correctness []Check     `yaml:"correctness" json:"correctness"`
	Quality     []Check     `yaml:"quality" json:"quality"`
	Replay      *ReplaySpec `yaml:"replay,omitempty" json:"replay,omitempty"`
}

// Check is one verification step. Fields are explicit on purpose: the verifier
// dispatches on Type and reads only the fields relevant to it, and validation
// rejects checks whose required fields are missing.
type Check struct {
	ID       string       `yaml:"id" json:"id"`
	Type     string       `yaml:"type" json:"type"`
	Title    string       `yaml:"title,omitempty" json:"title,omitempty"`
	Service  string       `yaml:"service,omitempty" json:"service,omitempty"`
	Path     string       `yaml:"path,omitempty" json:"path,omitempty"`
	Method   string       `yaml:"method,omitempty" json:"method,omitempty"`
	Query    string       `yaml:"query,omitempty" json:"query,omitempty"`
	SQL      string       `yaml:"sql,omitempty" json:"sql,omitempty"`
	Pattern  string       `yaml:"pattern,omitempty" json:"pattern,omitempty"`
	Rule     string       `yaml:"rule,omitempty" json:"rule,omitempty"`
	Command  []string     `yaml:"command,omitempty" json:"command,omitempty"`
	Duration string       `yaml:"duration,omitempty" json:"duration,omitempty"`
	Expect   *Expectation `yaml:"expect,omitempty" json:"expect,omitempty"`
	Reason   string       `yaml:"reason,omitempty" json:"reason,omitempty"`
	// File is the configuration file inside the container a policy check
	// reads (e.g. /etc/app/app.conf). Declaring it makes the check read the
	// file the service actually loads at startup instead of the process
	// environment, which is what keeps the "fix is visible to the check"
	// invariant honest when the fix is a config edit.
	File string `yaml:"file,omitempty" json:"file,omitempty"`
	// Agg is the aggregate function for metric checks over a sampled series:
	// max|min|avg|last|p95|slope|delta|resets. When set, the check reads from
	// the replay sampler instead of a single scrape.
	Agg string `yaml:"agg,omitempty" json:"agg,omitempty"`
	// Window is the observation slice for metric/replay checks. From skips the
	// warmup (e.g. "5s").
	Window *Window `yaml:"window,omitempty" json:"window,omitempty"`
	// Absent is for log checks: when true, the check passes when NO line
	// matches Pattern (e.g. no secrets in logs).
	Absent bool `yaml:"absent,omitempty" json:"absent,omitempty"`
	// Field is for replay checks: error_rate|success_rate|p99_ms|rps|errors_reset.
	Field string `yaml:"field,omitempty" json:"field,omitempty"`
	// Class narrows a replay check to the request class declared in the
	// workload profile (for example orders or healthz).
	Class string `yaml:"class,omitempty" json:"class,omitempty"`
}

// Window is the observation slice of a replay. From skips the warmup.
type Window struct {
	From string `yaml:"from,omitempty" json:"from,omitempty"` // e.g. "5s"
}

// Expectation is the pass condition of a check.
type Expectation struct {
	Status  *int     `yaml:"status,omitempty" json:"status,omitempty"`
	Equals  *string  `yaml:"equals,omitempty" json:"equals,omitempty"`
	Matches *string  `yaml:"matches,omitempty" json:"matches,omitempty"`
	Min     *float64 `yaml:"min,omitempty" json:"min,omitempty"`
	Max     *float64 `yaml:"max,omitempty" json:"max,omitempty"`
}

// RootCause states the true cause and the artefacts that prove it.
type RootCause struct {
	Summary  string   `yaml:"summary" json:"summary"`
	Layer    string   `yaml:"layer" json:"layer"`
	Signals  []string `yaml:"signals,omitempty" json:"signals,omitempty"`
	MaskedBy []string `yaml:"masked_by,omitempty" json:"masked_by,omitempty"`
}

// ReferenceSolution is the answer key shown in the postmortem.
type ReferenceSolution struct {
	Summary  string   `yaml:"summary" json:"summary"`
	Steps    []string `yaml:"steps,omitempty" json:"steps,omitempty"`
	Commands []string `yaml:"commands,omitempty" json:"commands,omitempty"`
	Rollback string   `yaml:"rollback,omitempty" json:"rollback,omitempty"`
}

// ReplaySpec configures the load replayed during verification.
type ReplaySpec struct {
	Profile       string       `yaml:"profile" json:"profile"`
	Duration      string       `yaml:"duration" json:"duration"`
	RPS           int          `yaml:"rps,omitempty" json:"rps,omitempty"`
	Concurrency   int          `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
	TargetService string       `yaml:"target_service,omitempty" json:"target_service,omitempty"`
	Hooks         []ReplayHook `yaml:"hooks,omitempty" json:"hooks,omitempty"`
}

// ReplayHook performs an action at an offset in a replay.
type ReplayHook struct {
	At      string            `yaml:"at" json:"at"`
	Do      string            `yaml:"do" json:"do"`
	Service string            `yaml:"service,omitempty" json:"service,omitempty"`
	Command []string          `yaml:"command,omitempty" json:"command,omitempty"`
	SQL     string            `yaml:"sql,omitempty" json:"sql,omitempty"`
	Vars    map[string]string `yaml:"vars,omitempty" json:"vars,omitempty"`
	Body    string            `yaml:"body,omitempty" json:"body,omitempty"`
}

// Distractor describes an action that hides the symptom without removing the
// root cause. It is what makes the quality gate meaningful.
type Distractor struct {
	ID             string           `yaml:"id" json:"id"`
	Title          string           `yaml:"title" json:"title"`
	Symptom        string           `yaml:"symptom" json:"symptom"`
	MaskedArtifact string           `yaml:"masked_artifact" json:"masked_artifact"`
	Fails          string           `yaml:"fails" json:"fails"`
	Apply          *DistractorApply `yaml:"apply,omitempty" json:"apply,omitempty"`
}

// DistractorApply is the data-driven action a distractor performs, replacing
// the old switch-based applyDistractor in cmd/incidentsim.
type DistractorApply struct {
	// Kind is one of env|files|sql|exec|restart|resources.
	Kind    string            `yaml:"kind" json:"kind"`
	Service string            `yaml:"service,omitempty" json:"service,omitempty"`
	Vars    map[string]string `yaml:"vars,omitempty" json:"vars,omitempty"`
	// From is a directory under scenarios/<id>/distractors/<did>/ whose files
	// are copied to `to` in the workspace.
	From string `yaml:"from,omitempty" json:"from,omitempty"`
	// To is the destination file in the workspace (for kind=files).
	To string `yaml:"to,omitempty" json:"to,omitempty"`
	// SQL statements to run (for kind=sql).
	SQL []string `yaml:"sql,omitempty" json:"sql,omitempty"`
	// Limits are resource limits (for kind=resources).
	Limits map[string]string `yaml:"limits,omitempty" json:"limits,omitempty"`
}
