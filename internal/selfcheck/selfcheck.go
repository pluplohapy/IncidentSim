// Package selfcheck runs the selfcheck contract for a task against a running
// core API. It does not read any files from the repository: the reference fix
// and the distractor materials are fetched from the admin endpoint
// (INCIDENTSIM_ADMIN=1), so the client only needs network access to the core.
//
// Contract (B11):
//
//	(a) start   -> verify must be fail or partial
//	(b) apply reference patch -> verify must be pass
//	(c) reset   -> back to baseline
//	(d) each distractor -> partial_fix, and the failing check must be the one
//	   named in the distractor's fails field
//	(e) stop
//
// --engine selects compose|firecracker. Each step's duration is printed.
package selfcheck

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"incidentsim/internal/cli"
	"incidentsim/internal/verifier"
)

// Engine selects the runtime engine.
type Engine string

const (
	EngineCompose   Engine = "compose"
	EngineFirecrack Engine = "firecracker"
)

// Result is the outcome of one selfcheck run.
type Result struct {
	TaskID    string        `json:"task_id"`
	Engine    string        `json:"engine"`
	StartOK   bool          `json:"start_ok"`
	StartTime time.Duration `json:"start_time"`
	BreakOK   bool          `json:"break_ok"`
	BreakTime time.Duration `json:"break_time"`
	FixOK     bool          `json:"fix_ok"`
	FixTime   time.Duration `json:"fix_time"`
	PassOK    bool          `json:"pass_ok"`
	PassTime  time.Duration `json:"pass_time"`
	ResetOK   bool          `json:"reset_ok"`
	ResetTime time.Duration `json:"reset_time"`
	// DistractorResults is one entry per distractor.
	DistractorResults []DistractorResult `json:"distractor_results"`
	StopOK            bool               `json:"stop_ok"`
	StopTime          time.Duration      `json:"stop_time"`
	Overall           bool               `json:"overall"`
	Err               error              `json:"err,omitempty"`
}

// DistractorResult is the outcome of one distractor selfcheck step.
type DistractorResult struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Fails       string        `json:"fails"`
	OK          bool          `json:"ok"`
	Time        time.Duration `json:"time"`
	ActualFails string        `json:"actual_fails,omitempty"`
	Err         error         `json:"err,omitempty"`
}

// Contract runs the full selfcheck contract for one task.
func Contract(ctx context.Context, api *cli.Client, engine Engine, taskID string) (*Result, error) {
	res := &Result{TaskID: taskID, Engine: string(engine)}

	// Ensure the task exists.
	if err := ensureTask(ctx, api, taskID); err != nil {
		res.Err = fmt.Errorf("task %q not in catalog: %w", taskID, err)
		return res, res.Err
	}

	// Read the full task (including distractors and reference) from the admin
	// endpoint. Selfcheck requires INCIDENTSIM_ADMIN=1.
	task, err := readAdminTask(ctx, api, taskID)
	if err != nil {
		res.Err = fmt.Errorf("read task %q: %w", taskID, err)
		return res, res.Err
	}
	if len(task.Distractors) == 0 {
		res.Err = fmt.Errorf("task %q declares no distractors", taskID)
		return res, res.Err
	}

	// (a) Start.
	start := time.Now()
	sessionID, err := api.Start(ctx, taskID)
	res.StartTime = time.Since(start)
	res.StartOK = err == nil
	if err != nil {
		res.Err = fmt.Errorf("start: %w", err)
		return res, res.Err
	}
	defer func() { _ = api.Stop(ctx, sessionID) }()

	// (a) Verify on the broken baseline: must be fail or partial, not pass.
	start = time.Now()
	report, err := verifyOnce(ctx, api, sessionID)
	res.BreakTime = time.Since(start)
	res.BreakOK = err == nil && (report.Verdict == verifier.VerdictFail || report.Verdict == verifier.VerdictPartial)
	if !res.BreakOK {
		res.Err = fmt.Errorf("baseline verify: verdict=%s, want fail|partial", report.Verdict)
		return res, res.Err
	}

	// (b) Apply the reference patch: verify must be pass.
	start = time.Now()
	if err := applyReference(ctx, api, sessionID, task); err != nil {
		res.FixTime = time.Since(start)
		res.Err = fmt.Errorf("apply reference: %w", err)
		return res, res.Err
	}
	res.FixTime = time.Since(start)
	res.FixOK = true

	start = time.Now()
	report, err = verifyOnce(ctx, api, sessionID)
	res.PassTime = time.Since(start)
	res.PassOK = err == nil && report.Verdict == verifier.VerdictPass
	if !res.PassOK {
		res.Err = fmt.Errorf("reference verify: verdict=%s, want pass", report.Verdict)
		return res, res.Err
	}

	// (c) Reset back to baseline.
	start = time.Now()
	if err := api.Reset(ctx, sessionID); err != nil {
		res.ResetTime = time.Since(start)
		res.Err = fmt.Errorf("reset: %w", err)
		return res, res.Err
	}
	res.ResetTime = time.Since(start)
	res.ResetOK = true

	// (d) Each distractor: apply -> partial_fix with the right failing check.
	for _, d := range task.Distractors {
		dr := DistractorResult{ID: d.ID, Title: d.Title, Fails: d.Fails}
		start = time.Now()
		if err := applyDistractor(ctx, api, sessionID, d); err != nil {
			dr.Time = time.Since(start)
			dr.Err = fmt.Errorf("apply distractor %q: %w", d.ID, err)
			res.DistractorResults = append(res.DistractorResults, dr)
			res.Err = fmt.Errorf("distractor %q apply: %w", d.ID, err)
			continue
		}
		dr.Time = time.Since(start)

		start = time.Now()
		report, err = verifyOnce(ctx, api, sessionID)
		dr.Time += time.Since(start)
		if err != nil {
			dr.Err = fmt.Errorf("verify distractor %q: %w", d.ID, err)
			res.DistractorResults = append(res.DistractorResults, dr)
			continue
		}
		if report.Verdict != verifier.VerdictPartial {
			dr.Err = fmt.Errorf("distractor %q: verdict=%s, want partial_fix", d.ID, report.Verdict)
			res.DistractorResults = append(res.DistractorResults, dr)
			continue
		}
		// The failing quality check must be the one named in fails.
		failedIDs := failedCheckIDs(report.Quality.Results)
		ok := len(failedIDs) == 1 && failedIDs[0] == d.Fails
		dr.OK = ok
		dr.ActualFails = strings.Join(failedIDs, ",")
		if !ok {
			dr.Err = fmt.Errorf("distractor %q: failing checks=%v, want %q", d.ID, failedIDs, d.Fails)
		}
		res.DistractorResults = append(res.DistractorResults, dr)

		// Reset after each distractor.
		if err := api.Reset(ctx, sessionID); err != nil {
			res.Err = fmt.Errorf("reset after distractor %q: %w", d.ID, err)
			return res, res.Err
		}
	}

	// (e) Stop.
	start = time.Now()
	if err := api.Stop(ctx, sessionID); err != nil {
		res.StopTime = time.Since(start)
		res.Err = fmt.Errorf("stop: %w", err)
		return res, res.Err
	}
	res.StopTime = time.Since(start)
	res.StopOK = true

	res.Overall = res.StartOK && res.BreakOK && res.FixOK && res.PassOK && res.ResetOK && res.StopOK
	for _, dr := range res.DistractorResults {
		if !dr.OK {
			res.Overall = false
		}
	}
	return res, nil
}

// Print prints a human-readable selfcheck result.
func (r *Result) Print(out io.Writer) {
	fmt.Fprintf(out, "=== selfcheck %s (engine=%s) ===\n", r.TaskID, r.Engine)
	fmt.Fprintf(out, "start:        %v  %s\n", r.StartTime, boolWord(r.StartOK))
	fmt.Fprintf(out, "baseline verify: %v  %s\n", r.BreakTime, boolWord(r.BreakOK))
	fmt.Fprintf(out, "apply reference: %v  %s\n", r.FixTime, boolWord(r.FixOK))
	fmt.Fprintf(out, "reference verify: %v  %s\n", r.PassTime, boolWord(r.PassOK))
	fmt.Fprintf(out, "reset:          %v  %s\n", r.ResetTime, boolWord(r.ResetOK))
	for _, dr := range r.DistractorResults {
		fmt.Fprintf(out, "  distractor %s (%s, fails %s): %v  %s", dr.ID, dr.Title, dr.Fails, dr.Time, boolWord(dr.OK))
		if dr.Err != nil {
			fmt.Fprintf(out, " -> %v", dr.Err)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "stop:           %v  %s\n", r.StopTime, boolWord(r.StopOK))
	if r.Err != nil {
		fmt.Fprintf(out, "ERROR: %v\n", r.Err)
	}
	fmt.Fprintf(out, "=== selfcheck %s: %s ===\n", r.TaskID, boolWord(r.Overall))
}

func boolWord(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// ensureTask checks that the task exists in the catalog.
func ensureTask(ctx context.Context, api *cli.Client, taskID string) error {
	var response struct {
		Tasks []struct{ ID string } `json:"tasks"`
	}
	if err := api.Get(ctx, "/api/v1/tasks", &response); err != nil {
		return err
	}
	for _, t := range response.Tasks {
		if t.ID == taskID {
			return nil
		}
	}
	return fmt.Errorf("task %q not found", taskID)
}

// readAdminTask reads the full (private) task detail from the admin endpoint.
func readAdminTask(ctx context.Context, api *cli.Client, taskID string) (*taskDetail, error) {
	var task taskDetail
	if err := api.Get(ctx, "/api/v1/admin/tasks/"+taskID, &task); err != nil {
		return nil, err
	}
	if task.Level == "" {
		return nil, fmt.Errorf("task %q declares no level", taskID)
	}
	return &task, nil
}

// verifyOnce runs one verification and returns the report.
func verifyOnce(ctx context.Context, api *cli.Client, sessionID string) (*verifier.Report, error) {
	var report verifier.Report
	if err := api.Post(ctx, "/api/v1/sessions/"+sessionID+"/verify", nil, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// applyReference applies the reference solution based on task level.
func applyReference(ctx context.Context, api *cli.Client, sessionID string, task *taskDetail) error {
	projectRoot := os.Getenv("INCIDENTSIM_PROJECT_ROOT")
	if projectRoot == "" {
		projectRoot = "."
	}
	referenceDir := referenceDirFor(projectRoot, *task)
	return applyReferenceFix(api, sessionID, *task, referenceDir)
}

// applyDistractor applies a distractor via the data-driven API.
func applyDistractor(ctx context.Context, api *cli.Client, sessionID string, d distractorDetail) error {
	_, err := api.Distractor(ctx, sessionID, d.ID)
	return err
}

// failedCheckIDs returns the ids of failed quality checks.
func failedCheckIDs(results []verifier.Result) []string {
	var out []string
	for _, r := range results {
		if !r.Passed {
			out = append(out, r.ID)
		}
	}
	return out
}

// taskDetail mirrors cli.taskDetail but is defined here to avoid importing cli
// internals (the admin endpoint returns the same shape).
type taskDetail struct {
	ID          string             `json:"id"`
	Level       string             `json:"level"`
	Editable    []string           `json:"editable,omitempty"`
	Reference   string             `json:"reference,omitempty"`
	Distractors []distractorDetail `json:"distractors,omitempty"`
}

type distractorDetail struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Fails string `json:"fails"`
	Apply struct {
		Kind    string            `json:"kind,omitempty"`
		Service string            `json:"service,omitempty"`
		Vars    map[string]string `json:"vars,omitempty"`
		From    string            `json:"from,omitempty"`
		To      string            `json:"to,omitempty"`
		SQL     []string          `json:"sql,omitempty"`
		Limits  map[string]string `json:"limits,omitempty"`
	} `json:"apply,omitempty"`
}

// referenceDirFor resolves the reference directory from the task detail.
func referenceDirFor(projectRoot string, task taskDetail) string {
	if task.Reference != "" {
		return projectRoot + "/" + task.Reference
	}
	return projectRoot + "/scenarios/" + task.ID + "/reference"
}

// applyReferenceFix applies the reference solution based on task level.
// This duplicates the logic in cmd/incidentsim.main.go but lives here so the
// selfcheck package has no dependency on the CLI binary.
func applyReferenceFix(api *cli.Client, sessionID string, task taskDetail, referenceDir string) error {
	service := "app"
	switch task.Level {
	case "code":
		if len(task.Editable) == 0 {
			return fmt.Errorf("code-level task %q has no editable files", task.ID)
		}
		refPath := task.Editable[0]
		content, err := readReferenceFile(referenceDir, refPath)
		if err != nil {
			return fmt.Errorf("read reference %s: %w", refPath, err)
		}
		return api.Apply(context.Background(), sessionID, service, nil, map[string]string{refPath: content}, true)
	case "config":
		if len(task.Editable) == 0 {
			return fmt.Errorf("config-level task %q has no editable files", task.ID)
		}
		refPath := task.Editable[0]
		content, err := readReferenceFile(referenceDir, refPath)
		if err != nil {
			return fmt.Errorf("read reference %s: %w", refPath, err)
		}
		return api.Apply(context.Background(), sessionID, service, nil, map[string]string{refPath: content}, true)
	case "db":
		content, err := readReferenceFile(referenceDir, "solution.sql")
		if err != nil {
			return fmt.Errorf("read reference solution.sql: %w", err)
		}
		for _, stmt := range splitSQLStatements(content) {
			if err := api.ExecSQL(context.Background(), sessionID, "db", stmt); err != nil {
				return fmt.Errorf("apply reference statement %q: %w", stmt, err)
			}
		}
		return nil
	case "composite":
		if len(task.Editable) == 0 {
			return fmt.Errorf("composite-level task %q has no editable files", task.ID)
		}
		for _, refPath := range task.Editable {
			content, err := readReferenceFile(referenceDir, refPath)
			if err != nil {
				return fmt.Errorf("read reference %s: %w", refPath, err)
			}
			if err := api.Apply(context.Background(), sessionID, service, nil, map[string]string{refPath: content}, true); err != nil {
				return err
			}
		}
		if content, err := readReferenceFile(referenceDir, "solution.sql"); err == nil {
			for _, stmt := range splitSQLStatements(content) {
				if err := api.ExecSQL(context.Background(), sessionID, "db", stmt); err != nil {
					return fmt.Errorf("apply reference statement %q: %w", stmt, err)
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown level %q", task.Level)
	}
}

// readReferenceFile reads a file from the reference directory.
func readReferenceFile(referenceDir, refPath string) (string, error) {
	data, err := os.ReadFile(referenceDir + "/" + refPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// splitSQLStatements splits a reference .sql file into individual statements.
func splitSQLStatements(content string) []string {
	var out []string
	for _, chunk := range strings.Split(content, ";") {
		var lines []string
		for _, line := range strings.Split(chunk, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				continue
			}
			lines = append(lines, line)
		}
		stmt := strings.TrimSpace(strings.Join(lines, "\n"))
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}
