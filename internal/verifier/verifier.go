// Package verifier grades a session on two levels.
//
// Correctness answers "does the system work now". Quality answers "is the fix
// appropriate for production". A run is green only when both gates pass; that
// is what separates IncidentSim from a checklist of endpoints.
package verifier

import (
	"context"
	"errors"
	"fmt"

	"incidentsim/internal/runtime"
)

// Verdicts of a gate.
const (
	VerdictPass    = "pass"
	VerdictFail    = "fail"
	VerdictSkipped = "skipped"
	VerdictPartial = "partial_fix"
)

// Errors of the verifier subsystem.
var (
	// ErrNoChecks means a manifest declared no checks for a gate.
	ErrNoChecks = errors.New("no checks declared")
	// ErrProbeFailed means the verifier could not reach the environment at
	// all, which is a session failure rather than a wrong answer.
	ErrProbeFailed = errors.New("unable to probe environment")
)

// CheckKind mirrors the manifest check types.
type CheckKind string

// Kinds of checks the verifier can run.
const (
	CheckHTTP   CheckKind = "http"
	CheckMetric CheckKind = "metric"
	CheckSQL    CheckKind = "sql"
	CheckLog    CheckKind = "log"
	CheckPolicy CheckKind = "policy"
	CheckExec   CheckKind = "exec"
	CheckReplay CheckKind = "replay"
	CheckUnit   CheckKind = "unit"
)

// Gate identifies which level a check belongs to.
type Gate string

// Gates of the verification.
const (
	GateCorrectness Gate = "correctness"
	GateQuality     Gate = "quality"
)

// Check is one graded step of a manifest, projected for the verifier.
type Check struct {
	ID       string       `json:"id"`
	Kind     CheckKind    `json:"kind"`
	Gate     Gate         `json:"gate"`
	Title    string       `json:"title,omitempty"`
	Service  string       `json:"service,omitempty"`
	Path     string       `json:"path,omitempty"`
	Method   string       `json:"method,omitempty"`
	Query    string       `json:"query,omitempty"`
	SQL      string       `json:"sql,omitempty"`
	Pattern  string       `json:"pattern,omitempty"`
	Rule     string       `json:"rule,omitempty"`
	Command  []string     `json:"command,omitempty"`
	Duration string       `json:"duration,omitempty"`
	Expect   *Expectation `json:"expect,omitempty"`
	Reason   string       `json:"reason,omitempty"`
	// File is the configuration file inside the container a policy check
	// reads. When set, the check reads that file instead of the process
	// environment: the file is what the service loads at startup, so a fix
	// that edits the file must be visible to the check and one that only
	// touches the environment must not be.
	File string `json:"file,omitempty"`
	// Agg is the aggregate function for metric checks over a sampled series:
	// max|min|avg|last|p95|slope|delta|resets. When set, the check reads from
	// the replay sampler instead of a single scrape.
	Agg string `json:"agg,omitempty"`
	// Window is the observation slice for metric/replay checks. From skips the
	// warmup (e.g. "5s").
	Window *Window `json:"window,omitempty"`
	// Absent is for log checks: when true, the check passes when NO line
	// matches Pattern (e.g. no secrets in logs).
	Absent bool `json:"absent,omitempty"`
	// Field is for replay checks: error_rate|success_rate|p99_ms|rps|errors_reset.
	Field string `json:"field,omitempty"`
	// Class narrows a replay check to one workload request class.
	Class string `json:"class,omitempty"`
}

// Window is the observation slice of a replay. From skips the warmup.
type Window struct {
	From string `json:"from,omitempty"` // e.g. "5s"
}

// Expectation is the pass condition.
type Expectation struct {
	Status  *int     `json:"status,omitempty"`
	Equals  *string  `json:"equals,omitempty"`
	Matches *string  `json:"matches,omitempty"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
}

// Result is the outcome of one check.
type Result struct {
	Check  `json:"check"`
	Passed bool `json:"passed"`
	// Detail explains the outcome: expected versus observed.
	Detail string `json:"detail"`
	// Evidence is the raw observation the verdict came from, kept for the
	// postmortem and for debugging a task that grades oddly.
	Evidence string `json:"evidence,omitempty"`
	// Duration is how long the check took.
	Duration string `json:"duration,omitempty"`
}

// GateResult aggregates the checks of one level.
type GateResult struct {
	Gate    Gate     `json:"gate"`
	Verdict string   `json:"verdict"`
	Results []Result `json:"results"`
}

// Passed reports whether every check of the gate passed.
func (g GateResult) Passed() bool { return g.Verdict == VerdictPass }

// FailedResults returns the checks that did not pass.
func (g GateResult) FailedResults() []Result {
	var out []Result
	for _, result := range g.Results {
		if !result.Passed {
			out = append(out, result)
		}
	}
	return out
}

// Report is the complete outcome of a verification run.
type Report struct {
	SessionID   string     `json:"session_id"`
	TaskID      string     `json:"task_id"`
	Correctness GateResult `json:"correctness"`
	Quality     GateResult `json:"quality"`
	// Verdict is pass only when both gates pass.
	Verdict string `json:"verdict"`
	// Replay summarises the load that was replayed during the run.
	Replay string `json:"replay,omitempty"`
	// Notes carries warnings that did not fail the run, e.g. a skipped check.
	Notes []string `json:"notes,omitempty"`
}

// Passed reports whether the whole run is green.
func (r Report) Passed() bool { return r.Verdict == VerdictPass }

// Environment is what a verifier may read and run. Keeping it narrow is what
// makes the "distractor fails quality" test possible without a real engine.
type Environment interface {
	// InstanceID returns the session being verified.
	InstanceID() string
	// Instance returns the live environment, which is how a replay runner
	// finds the endpoints to load.
	Instance() *runtime.Instance
	// Runtime exposes the adapter for logs, metrics and exec.
	Runtime() runtime.Adapter
	// Env returns the effective environment of a service, which is how a
	// policy check sees a masked symptom.
	Env(ctx context.Context, service string) (map[string]string, error)
	// SQL runs a statement against a service and returns its rows.
	SQL(ctx context.Context, service, statement string) ([]map[string]string, error)
	// WorkspaceDir returns the participant workspace dir, or "" if none.
	WorkspaceDir() string
}

// ReplayConfig is the load a verification replays. Replaying load is what keeps
// a task from being "fixed" by stopping the service.
type ReplayConfig struct {
	Profile       string `json:"profile,omitempty"`
	Duration      string `json:"duration,omitempty"`
	RPS           int    `json:"rps,omitempty"`
	Concurrency   int    `json:"concurrency,omitempty"`
	TargetService string `json:"target_service,omitempty"`
}

// Verifier grades one run. The first implementation runs manifest checks; the
// interface exists so alternative graders (manual review, an external grader)
// can be swapped in without touching the core.
type Verifier interface {
	// Verify runs both gates and returns the full report.
	Verify(ctx context.Context, env Environment, sessionID, taskID string, checks CheckSet) (Report, error)
}

// CheckSet groups the checks of a manifest by gate and carries the replay
// configuration the task declared.
type CheckSet struct {
	Correctness []Check
	Quality     []Check
	Replay      *ReplayConfig
}

// Empty reports whether the set declares no checks at all.
func (s CheckSet) Empty() bool { return len(s.Correctness) == 0 && len(s.Quality) == 0 }

// ReplayTargetService returns the target service for replay, preferring the
// explicit replay.target_service from the manifest.
func (s CheckSet) ReplayTargetService() string {
	if s.Replay != nil {
		return s.Replay.TargetService
	}
	return ""
}

// NewReport builds an empty report for a session.
func NewReport(sessionID, taskID string) Report {
	return Report{
		SessionID: sessionID,
		TaskID:    taskID,
		Correctness: GateResult{
			Gate:    GateCorrectness,
			Verdict: VerdictSkipped,
		},
		Quality: GateResult{
			Gate:    GateQuality,
			Verdict: VerdictSkipped,
		},
		Verdict: VerdictSkipped,
	}
}

// Finalise computes gate verdicts and the overall verdict of a report.
func (r *Report) Finalise() {
	r.Correctness.Verdict = gateVerdict(r.Correctness)
	r.Quality.Verdict = gateVerdict(r.Quality)
	switch {
	case r.Correctness.Passed() && r.Quality.Passed():
		r.Verdict = VerdictPass
	case !r.Correctness.Passed():
		r.Verdict = VerdictFail
	default:
		// Correctness passes but quality fails: the symptom is hidden but the
		// root cause survives. This is a partial fix, not a green run.
		r.Verdict = VerdictPartial
	}
}

func gateVerdict(gate GateResult) string {
	if len(gate.Results) == 0 {
		return VerdictSkipped
	}
	for _, result := range gate.Results {
		if !result.Passed {
			return VerdictFail
		}
	}
	return VerdictPass
}

// String renders the report for the CLI and for logs.
func (r Report) String() string {
	return fmt.Sprintf("verdict=%s correctness=%s quality=%s", r.Verdict, r.Correctness.Verdict, r.Quality.Verdict)
}
