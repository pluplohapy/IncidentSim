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
}

// ReplayConfig is the load a verification replays. Replaying load is what keeps
// a task from being "fixed" by stopping the service.
type ReplayConfig struct {
	Profile     string `json:"profile,omitempty"`
	Duration    string `json:"duration,omitempty"`
	RPS         int    `json:"rps,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
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
		// The environment works but the fix is not production appropriate:
		// the run is a partial fix, which is exactly what the quality gate
		// exists to expose.
		r.Verdict = VerdictFail
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
