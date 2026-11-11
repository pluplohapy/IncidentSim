// Package cli is the thin client of the IncidentSim core. It holds no task
// logic: it talks HTTP to the core API so that a participant could drive the
// same simulator from another client.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"incidentsim/internal/manifest"
	"incidentsim/internal/runtime"
	"incidentsim/internal/verifier"
)

// Client is the HTTP client of the core API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Out and Err are the writers the commands use.
	Out io.Writer
	Err io.Writer
}

// New returns a client for the given base URL.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
		Out:     os.Stdout,
		Err:     os.Stderr,
	}
}

// ErrAPI is returned when the core answers with an error envelope.
type ErrAPI struct {
	Status  int
	Message string
	Detail  string
}

func (e *ErrAPI) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("core returned %d: %s (%s)", e.Status, e.Message, e.Detail)
	}
	return fmt.Sprintf("core returned %d: %s", e.Status, e.Message)
}

// Get performs a GET against the core API. It is exported so the demo command
// can read the catalog without duplicating the request path.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.call(ctx, http.MethodGet, path, nil, out)
}

// Post performs a POST against the core API. Exported for use by selfcheck.
func (c *Client) Post(ctx context.Context, path string, body any, out any) error {
	return c.post(ctx, path, body, out)
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	return c.call(ctx, http.MethodPost, path, body, out)
}

func (c *Client) call(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the core at %s: %w (run `incidentsim serve` first)", c.BaseURL, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var envelope struct {
			Error   string `json:"error"`
			Details string `json:"details"`
		}
		if json.Unmarshal(payload, &envelope) == nil && envelope.Error != "" {
			return &ErrAPI{Status: resp.StatusCode, Message: envelope.Error, Detail: envelope.Details}
		}
		return &ErrAPI{Status: resp.StatusCode, Message: strings.TrimSpace(string(payload))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}

// Show lists the catalog, or dumps one task with its symptoms and theory.
func (c *Client) Show(ctx context.Context, taskID string) error {
	if taskID == "" {
		var response struct {
			Tasks []struct {
				ID               string   `json:"id"`
				Title            string   `json:"title"`
				Level            string   `json:"level"`
				Tags             []string `json:"tags"`
				EstimatedMinutes int      `json:"estimated_minutes"`
				Engine           string   `json:"engine"`
				Correctness      int      `json:"correctness_checks"`
				Quality          int      `json:"quality_checks"`
				Distractors      int      `json:"distractors"`
			} `json:"tasks"`
		}
		if err := c.Get(ctx, "/api/v1/tasks", &response); err != nil {
			return err
		}
		if len(response.Tasks) == 0 {
			fmt.Fprintln(c.Out, "catalog is empty")
			return nil
		}
		fmt.Fprintf(c.Out, "%-28s %-10s %-8s %s\n", "ID", "LEVEL", "CHECKS", "TITLE")
		for _, task := range response.Tasks {
			fmt.Fprintf(c.Out, "%-28s %-10s %-8s %s\n", task.ID, task.Level,
				fmt.Sprintf("%d/%d", task.Correctness, task.Quality), task.Title)
		}
		return nil
	}

	var task struct {
		ID        string             `json:"id"`
		Title     string             `json:"title"`
		Level     string             `json:"level"`
		Symptoms  []manifest.Symptom `json:"symptoms"`
		RootCause string             `json:"root_cause"`
		Theory    string             `json:"theory"`
	}
	if err := c.Get(ctx, "/api/v1/tasks/"+taskID, &task); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "%s — %s (%s)\n", task.ID, task.Title, task.Level)
	fmt.Fprintln(c.Out, "Симптомы:")
	for _, symptom := range task.Symptoms {
		fmt.Fprintf(c.Out, "  • %s: %s\n", symptom.ID, symptom.Observation)
	}
	fmt.Fprintf(c.Out, "\n%s\n", task.Theory)
	return nil
}

// Start begins a task and returns the new session id.
func (c *Client) Start(ctx context.Context, taskID string) (string, error) {
	var session struct {
		ID           string   `json:"id"`
		State        string   `json:"state"`
		Engine       string   `json:"engine"`
		Services     []string `json:"services"`
		WorkspaceDir string   `json:"workspace_dir,omitempty"`
		Editable     []string `json:"editable,omitempty"`
	}
	if err := c.post(ctx, "/api/v1/sessions", map[string]string{"task_id": taskID}, &session); err != nil {
		return "", err
	}
	fmt.Fprintf(c.Out, "session %s is %s on engine %s\n", session.ID, session.State, session.Engine)
	for _, service := range session.Services {
		fmt.Fprintf(c.Out, "  service %s\n", service)
	}
	if session.WorkspaceDir != "" {
		fmt.Fprintf(c.Out, "Твой код: %s\n", session.WorkspaceDir)
		if len(session.Editable) > 0 {
			fmt.Fprintf(c.Out, "Редактируемые файлы: %s\n", strings.Join(session.Editable, ", "))
		}
	}
	fmt.Fprintf(c.Out, "смотрите метрики: incidentsim metrics --session %s\n", session.ID)
	return session.ID, nil
}

// Env prints the effective environment of a service, which is how a
// participant finds a config-level root cause.
func (c *Client) Env(ctx context.Context, sessionID, service string) error {
	var response struct {
		Service string            `json:"service"`
		Env     map[string]string `json:"env"`
	}
	if err := c.Get(ctx, "/api/v1/sessions/"+sessionID+"/env?service="+service, &response); err != nil {
		return err
	}
	keys := make([]string, 0, len(response.Env))
	for key := range response.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(c.Out, "%s=%s\n", key, response.Env[key])
	}
	return nil
}

// Metrics prints the latest metrics of a session.
func (c *Client) Metrics(ctx context.Context, sessionID, service string) error {
	var response struct {
		Metrics []runtime.MetricSample `json:"metrics"`
	}
	if err := c.Get(ctx, "/api/v1/sessions/"+sessionID+"/metrics?service="+service, &response); err != nil {
		return err
	}
	if len(response.Metrics) == 0 {
		fmt.Fprintln(c.Out, "no metrics reported")
		return nil
	}
	for _, sample := range response.Metrics {
		fmt.Fprintf(c.Out, "%-32s %v\n", sample.Name, sample.Value)
	}
	return nil
}

// Logs prints the logs of a session, optionally grepped.
func (c *Client) Logs(ctx context.Context, sessionID, service, grep string, tail int) error {
	path := fmt.Sprintf("/api/v1/sessions/%s/logs?service=%s&tail=%d", sessionID, service, tail)
	if grep != "" {
		path += "&grep=" + grep
	}
	var response struct {
		Logs []runtime.LogLine `json:"logs"`
	}
	if err := c.Get(ctx, path, &response); err != nil {
		return err
	}
	for _, line := range response.Logs {
		fmt.Fprintf(c.Out, "%s %s %s\n", line.Timestamp.Format("15:04:05"), line.Service, line.Text)
	}
	return nil
}

// Hint asks for the next hint of a session.
func (c *Client) Hint(ctx context.Context, sessionID string, level int) error {
	path := fmt.Sprintf("/api/v1/sessions/%s/hint", sessionID)
	if level > 0 {
		path += fmt.Sprintf("?level=%d", level)
	}
	var response struct {
		Hint  manifest.Hint `json:"hint"`
		Found bool          `json:"found"`
	}
	if err := c.post(ctx, path, nil, &response); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "подсказка %d: %s\n", response.Hint.Level, response.Hint.Text)
	if !response.Found {
		fmt.Fprintln(c.Out, "(подсказки закончились)")
	}
	return nil
}

// Apply pushes a fix into the environment.
func (c *Client) Apply(ctx context.Context, sessionID, service string, vars map[string]string, files map[string]string, restart bool) error {
	var response struct {
		Applied runtime.EnvPatch `json:"applied"`
	}
	body := map[string]any{
		"service": service,
		"vars":    vars,
		"files":   files,
		"restart": restart,
	}
	if err := c.post(ctx, "/api/v1/sessions/"+sessionID+"/apply", body, &response); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "applied to %s: %v (restart=%v)\n", service, vars, restart)
	for path, content := range files {
		fmt.Fprintf(c.Out, "  wrote %s: %q\n", path, content)
	}
	return nil
}

// ExecSQL runs a statement on a database service. It is how a db-level fix is
// applied: the change is a statement, not a configuration variable.
// ExecSQL runs a statement through the core, which knows the database client
// of the task. It is sugar over Exec: the statement is turned into a command
// by the server, because the client binary and the task database are manifest
// data the core owns.
func (c *Client) ExecSQL(ctx context.Context, sessionID, service, statement string) error {
	result, err := c.exec(ctx, sessionID, service, nil, statement, 0)
	if err != nil {
		return err
	}
	return reportExecResult(c, result)
}

// Exec runs an arbitrary command inside a service of a session and prints its
// output. It is the participant's shell into the environment: `incidentsim
// exec --service app -- sh -c 'cat /etc/app/app.conf'` ends up here.
func (c *Client) Exec(ctx context.Context, sessionID, service string, command []string) error {
	result, err := c.exec(ctx, sessionID, service, command, "", 0)
	if err != nil {
		return err
	}
	return reportExecResult(c, result)
}

// exec posts either a command or a statement and returns the raw outcome, so
// both callers share one request path.
func (c *Client) exec(ctx context.Context, sessionID, service string, command []string, statement string, timeoutMS int) (ExecResult, error) {
	body := map[string]any{"service": service}
	if len(command) > 0 {
		body["command"] = command
	}
	if statement != "" {
		body["statement"] = statement
	}
	if timeoutMS > 0 {
		body["timeout_ms"] = timeoutMS
	}
	var response struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := c.post(ctx, "/api/v1/sessions/"+sessionID+"/exec", body, &response); err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: response.ExitCode, Stdout: response.Stdout, Stderr: response.Stderr}, nil
}

// ExecResult is the raw outcome of a command run inside a session.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// reportExecResult prints what the command produced and turns a non-zero exit
// into an error, so `incidentsim exec` behaves like the command it wraps.
func reportExecResult(c *Client, result ExecResult) error {
	if result.Stdout != "" {
		fmt.Fprintln(c.Out, strings.TrimSpace(result.Stdout))
	}
	if result.Stderr != "" {
		fmt.Fprintln(c.Err, strings.TrimSpace(result.Stderr))
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("command exited %d", result.ExitCode)
	}
	return nil
}

// Verify runs both gates and prints the report.
func (c *Client) Verify(ctx context.Context, sessionID string) error {
	var report verifier.Report
	if err := c.post(ctx, "/api/v1/sessions/"+sessionID+"/verify", nil, &report); err != nil {
		return err
	}
	printReport(c.Out, report)
	if report.Verdict != verifier.VerdictPass {
		return errors.New("verification did not pass")
	}
	return nil
}

// printReport renders a verification report for a terminal.
func printReport(out io.Writer, report verifier.Report) {
	fmt.Fprintf(out, "verdict: %s\n", strings.ToUpper(report.Verdict))
	if report.Replay != "" {
		fmt.Fprintf(out, "replay: %s\n", report.Replay)
	}
	for _, gate := range []verifier.GateResult{report.Correctness, report.Quality} {
		fmt.Fprintf(out, "\n%s gate: %s\n", gate.Gate, strings.ToUpper(gate.Verdict))
		for _, result := range gate.Results {
			mark := "PASS"
			if !result.Passed {
				mark = "FAIL"
			}
			fmt.Fprintf(out, "  [%s] %-24s %s\n", mark, result.Check.ID, result.Detail)
			if result.Evidence != "" {
				fmt.Fprintf(out, "         evidence: %s\n", truncate(result.Evidence, 160))
			}
			if !result.Passed && result.Check.Reason != "" {
				fmt.Fprintf(out, "         почему это важно: %s\n", result.Check.Reason)
			}
		}
	}
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// Postmortem prints the generated draft of a session report.
func (c *Client) Postmortem(ctx context.Context, sessionID string, markdown bool) error {
	path := "/api/v1/sessions/" + sessionID + "/postmortem"
	if markdown {
		path += "?format=md"
	}
	if markdown {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+path, nil)
		if err != nil {
			return err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return fmt.Errorf("cannot reach the core at %s: %w (run `incidentsim serve` first)", c.BaseURL, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 400 {
			return &ErrAPI{Status: resp.StatusCode, Message: strings.TrimSpace(string(body))}
		}
		fmt.Fprintln(c.Out, string(body))
		return nil
	}
	var postmortem struct {
		SessionID  string `json:"session_id"`
		Verdict    string `json:"verdict"`
		Duration   string `json:"duration"`
		HintsUsed  int    `json:"hints_used"`
		PartialFix bool   `json:"partial_fix"`
		Note       string `json:"partial_fix_note"`
		Timeline   []struct {
			At      time.Time `json:"at"`
			Type    string    `json:"type"`
			Message string    `json:"message"`
		} `json:"timeline"`
	}
	if err := c.Get(ctx, path, &postmortem); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "postmortem for %s (%s, %d hints)\n", postmortem.SessionID, postmortem.Duration, postmortem.HintsUsed)
	for _, entry := range postmortem.Timeline {
		fmt.Fprintf(c.Out, "  %s %-18s %s\n", entry.At.Format("15:04:05"), entry.Type, entry.Message)
	}
	if postmortem.PartialFix {
		fmt.Fprintf(c.Out, "\nЧАСТИЧНЫЙ ФИКС: %s\n", postmortem.Note)
	}
	return nil
}

// Reset restores the baseline environment of a session.
func (c *Client) Reset(ctx context.Context, sessionID string) error {
	if err := c.post(ctx, "/api/v1/sessions/"+sessionID+"/reset", nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "session %s restored to baseline\n", sessionID)
	return nil
}

// Diff prints the unified diff of the session workspace against the scenario.
func (c *Client) Diff(ctx context.Context, sessionID string) error {
	var response struct {
		Diff string `json:"diff"`
	}
	if err := c.Get(ctx, "/api/v1/sessions/"+sessionID+"/diff", &response); err != nil {
		return err
	}
	if response.Diff == "" {
		fmt.Fprintln(c.Out, "no changes")
		return nil
	}
	fmt.Fprint(c.Out, response.Diff)
	return nil
}

// Edit opens $EDITOR on a file of the session workspace. Without a file it
// prints the workspace path so the participant can open it in their editor.
func (c *Client) Edit(ctx context.Context, sessionID, file string) error {
	var session struct {
		Session struct {
			WorkspaceDir string `json:"workspace_dir,omitempty"`
		} `json:"session"`
	}
	if err := c.Get(ctx, "/api/v1/sessions/"+sessionID, &session); err != nil {
		return err
	}
	dir := session.Session.WorkspaceDir
	if dir == "" {
		return fmt.Errorf("session %s has no workspace", sessionID)
	}
	target := dir
	if file != "" {
		target = filepath.Join(dir, file)
		if _, err := os.Stat(target); err != nil {
			return fmt.Errorf("file %q not found in workspace: %w", file, err)
		}
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command(editor, target)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor %q: %w", editor, err)
	}
	fmt.Fprintf(c.Out, "workspace: %s\n", dir)
	return nil
}

// Shell opens an interactive terminal in a service of the session.
func (c *Client) Shell(ctx context.Context, sessionID, service string) error {
	command := []string{"sh"}
	result, err := c.execTTY(ctx, sessionID, service, command)
	if err != nil {
		return err
	}
	return reportExecResult(c, result)
}

// execTTY runs a command inside a service with a terminal allocated, for an
// interactive shell. It reuses the same request path as exec.
func (c *Client) execTTY(ctx context.Context, sessionID, service string, command []string) (ExecResult, error) {
	body := map[string]any{"service": service, "command": command, "tty": true}
	var response struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := c.post(ctx, "/api/v1/sessions/"+sessionID+"/exec", body, &response); err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: response.ExitCode, Stdout: response.Stdout, Stderr: response.Stderr}, nil
}
func (c *Client) Stop(ctx context.Context, sessionID string) error {
	if err := c.post(ctx, "/api/v1/sessions/"+sessionID+"/stop", nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "session %s stopped\n", sessionID)
	return nil
}

// Sessions lists the sessions of the core.
func (c *Client) Sessions(ctx context.Context) error {
	var response struct {
		Sessions []struct {
			ID          string `json:"id"`
			TaskID      string `json:"task_id"`
			State       string `json:"state"`
			LastVerdict string `json:"last_verdict"`
			Engine      string `json:"engine"`
		} `json:"sessions"`
	}
	if err := c.Get(ctx, "/api/v1/sessions", &response); err != nil {
		return err
	}
	if len(response.Sessions) == 0 {
		fmt.Fprintln(c.Out, "no sessions")
		return nil
	}
	for _, session := range response.Sessions {
		fmt.Fprintf(c.Out, "%-34s %-26s %-10s %s\n", session.ID, session.TaskID, session.State, session.LastVerdict)
	}
	return nil
}
