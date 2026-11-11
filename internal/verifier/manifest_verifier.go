package verifier

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"incidentsim/internal/runtime"
)

// ManifestVerifier grades a session by running the checks a manifest declares.
//
// It is the default grader and the reason the whole system exists: every check
// is data, so a task author controls exactly what "fixed" means without
// touching Go code.
//
// B6: Verify order is baseline reset -> correctness (point checks) ->
// replay+sampler (one replay, sampler scrapes /metrics every second) ->
// quality (aggregates from the sampler series). A single verify stays under 45 s.
type ManifestVerifier struct {
	// HTTPTimeout bounds every http check.
	HTTPTimeout time.Duration
	// ReplayRunner replays load before the correctness checks run. It is a
	// field so a test can substitute a runner with a controlled clock.
	ReplayRunner *ReplayRunner
	// Sampler scrapes /metrics during the replay. When nil a default sampler
	// is used.
	Sampler *Sampler
	// ProjectDir is the repository root the profiles are relative to, so a
	// manifest writes workloads/db-connection-leak.yaml exactly as the path
	// looks on disk. A task whose profile cannot be loaded fails loudly.
	ProjectDir string

	// taskID is set for the duration of a Verify call so checkUnit can locate
	// scenarios/<taskID>/checks. It is not safe for concurrent use.
	taskID string
}

// NewManifestVerifier returns the default grader with production timeouts.
func NewManifestVerifier() *ManifestVerifier {
	return &ManifestVerifier{
		HTTPTimeout:  15 * time.Second,
		ReplayRunner: &ReplayRunner{Now: time.Now},
		Sampler:      NewSampler(),
	}
}

// Verify implements Verifier.
//
// The order is the whole point of the method:
//
//   - the baseline counters of earlier runs are cleared first, so no
//     measurement of this run is polluted by a previous one;
//   - correctness runs before the load: it answers "does the system
//     answer at all", which is a property of the current state and not of
//     the replay;
//   - the replay runs once while the sampler scrapes /metrics every second
//     into time series;
//   - the quality gate measures aggregates from the sampler series (and the
//     replay result), never an idle system.
func (v *ManifestVerifier) Verify(ctx context.Context, env Environment, sessionID, taskID string, checks CheckSet) (Report, error) {
	v.taskID = taskID
	defer func() { v.taskID = "" }()
	report := NewReport(sessionID, taskID)
	if env == nil {
		return report, fmt.Errorf("%w: no environment given", ErrProbeFailed)
	}

	// Counters of a previous verification must not land inside the window
	// this one measures.
	v.resetBaseline(ctx, env)

	// Correctness first: point checks of the current state.
	report.Correctness.Results = v.runGate(ctx, env, checks.Correctness)

	// One replay while the sampler scrapes /metrics every second. The sampler
	// is started before the replay and stopped after, so the series covers the
	// whole observation window. A fresh sampler is used per Verify run so the
	// series is never polluted by a previous run.
	sampler := NewSampler()
	instance := env.Instance()
	target := ""
	if instance != nil {
		target = replayTarget(instance, checks)
	}
	replayCtx, cancelReplay := context.WithCancel(ctx)
	if target != "" {
		go sampler.Start(replayCtx, target, checks.ReplayTargetService())
	}
	replayResult := v.runReplay(replayCtx, env, checks)
	if target != "" {
		sampler.Stop()
	}
	cancelReplay()

	// Quality gate measures aggregates from the sampler series (and the
	// replay result).
	report.Quality.Results = v.runQualityGate(ctx, env, checks.Quality, sampler, replayResult)
	report.Replay = replayResult.String()
	report.Finalise()
	return report, nil
}

// resetBaseline clears the counters a service keeps across verifications.
//
// A metric like app_errors_total is a counter, so a window that measures it
// without a reset would grade the traffic of the previous run as well. The
// debug endpoint is a capability of the task service: a service that does
// not expose it simply keeps its counters, and a missing endpoint is not a
// failed run.
func (v *ManifestVerifier) resetBaseline(ctx context.Context, env Environment) {
	instance := env.Instance()
	if instance == nil {
		return
	}
	for _, svc := range instance.Services {
		if svc.Endpoint == "" || strings.HasPrefix(svc.Endpoint, "vsock://") {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+svc.Endpoint+"/debug/reset", nil)
		if err != nil {
			continue
		}
		client := &http.Client{Timeout: 2 * time.Second}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}
}

// runReplay replays the declared load and returns a summary for the report.
// A missing profile is a note, not a failure: a static task may have nothing to
// replay, and the gates still run against the current state.
//
// The replay targets the service the correctness checks measure. Picking the
// first service of the environment instead would replay against a database,
// which speaks no HTTP and reports a thousand meaningless errors.
func (v *ManifestVerifier) runReplay(ctx context.Context, env Environment, checks CheckSet) ReplayResult {
	config := checks.Replay
	if config == nil || config.Duration == "" {
		return ReplayResult{}
	}
	instance := env.Instance()
	if instance == nil {
		return ReplayResult{}
	}
	requests, err := v.profileRequests(config)
	if err != nil {
		return ReplayResult{}
	}
	target := replayTarget(instance, checks)
	if target == "" {
		return ReplayResult{}
	}
	result, err := v.ReplayRunner.Replay(ctx, target, requests, *config)
	if err != nil {
		return ReplayResult{}
	}
	return result
}

// replayTarget resolves the endpoint the replay should load.
func replayTarget(instance *runtime.Instance, checks CheckSet) string {
	// An explicit replay.target_service from the manifest takes precedence.
	if checks.Replay != nil && checks.Replay.TargetService != "" {
		if endpoint := endpointFor(instance, checks.Replay.TargetService); endpoint != "" {
			return endpoint
		}
	}
	// A correctness check names the service that carries the incident, so it is
	// the most honest source of the target.
	for _, check := range checks.Correctness {
		if check.Kind != CheckHTTP || check.Service == "" {
			continue
		}
		if endpoint := endpointFor(instance, check.Service); endpoint != "" {
			return endpoint
		}
	}
	// Otherwise the first service reachable over HTTP, which excludes a
	// database: a microVM behind vsock is reached through the guest agent.
	for _, service := range instance.Services {
		if strings.HasPrefix(service.Endpoint, "vsock://") || service.Endpoint == "" {
			continue
		}
		return service.Endpoint
	}
	return ""
}

// profileRequests loads the requests a replay profile declares.
func (v *ManifestVerifier) profileRequests(config *ReplayConfig) ([]Request, error) {
	if config.Profile == "" {
		return nil, fmt.Errorf("%w: the replay section declares no profile", ErrNoReplayProfile)
	}
	profile, err := LoadReplayProfile(v.ProjectDir, config.Profile)
	if err != nil {
		return nil, err
	}
	if config.Concurrency == 0 {
		config.Concurrency = profile.Concurrency
	}
	return profile.Requests, nil
}

// runQualityGate executes every quality check, using the sampler series for
// metric-with-agg checks and the replay result for replay checks.
func (v *ManifestVerifier) runQualityGate(ctx context.Context, env Environment, checks []Check, sampler *Sampler, replayResult ReplayResult) []Result {
	results := make([]Result, 0, len(checks))
	for _, check := range checks {
		results = append(results, v.runQualityCheck(ctx, env, check, sampler, replayResult))
	}
	return results
}

func (v *ManifestVerifier) runQualityCheck(ctx context.Context, env Environment, check Check, sampler *Sampler, replayResult ReplayResult) Result {
	start := time.Now()
	result := Result{Check: check}
	if env == nil {
		result.Passed = false
		result.Detail = "unable to probe the environment: no environment given"
		result.Duration = time.Since(start).Round(time.Millisecond).String()
		return result
	}

	switch check.Kind {
	case CheckHTTP:
		result = v.checkHTTP(ctx, env, check)
	case CheckMetric:
		result = v.checkMetric(ctx, env, check, sampler)
	case CheckLog:
		result = v.checkLog(ctx, env, check)
	case CheckExec:
		result = v.checkExec(ctx, env, check)
	case CheckSQL:
		result = v.checkSQL(ctx, env, check)
	case CheckPolicy:
		result = v.checkPolicy(ctx, env, check)
	case CheckReplay:
		result = v.checkReplay(ctx, env, check, replayResult)
	case CheckUnit:
		result = v.checkUnit(ctx, env, check, v.taskID)
	default:
		result.Passed = false
		result.Detail = fmt.Sprintf("unknown check kind %q", check.Kind)
	}
	result.Duration = time.Since(start).Round(time.Millisecond).String()
	return result
}

// runGate executes every check of one gate and never stops early: a report
// must show all failures, because the first failing check is rarely the only
// thing that is wrong with a fix.
func (v *ManifestVerifier) runGate(ctx context.Context, env Environment, checks []Check) []Result {
	results := make([]Result, 0, len(checks))
	for _, check := range checks {
		results = append(results, v.runCheck(ctx, env, check))
	}
	return results
}

func (v *ManifestVerifier) runCheck(ctx context.Context, env Environment, check Check) Result {
	start := time.Now()
	result := Result{Check: check}
	if env == nil {
		// A check without an environment cannot observe anything, and a pass
		// here would be a false green.
		result.Passed = false
		result.Detail = "unable to probe the environment: no environment given"
		result.Duration = time.Since(start).Round(time.Millisecond).String()
		return result
	}

	switch check.Kind {
	case CheckHTTP:
		result = v.checkHTTP(ctx, env, check)
	case CheckMetric:
		result = v.checkMetric(ctx, env, check, nil)
	case CheckLog:
		result = v.checkLog(ctx, env, check)
	case CheckExec:
		result = v.checkExec(ctx, env, check)
	case CheckSQL:
		result = v.checkSQL(ctx, env, check)
	case CheckPolicy:
		result = v.checkPolicy(ctx, env, check)
	case CheckUnit:
		result = v.checkUnit(ctx, env, check, v.taskID)
	default:
		result.Passed = false
		result.Detail = fmt.Sprintf("unknown check kind %q", check.Kind)
	}
	result.Duration = time.Since(start).Round(time.Millisecond).String()
	return result
}

func (v *ManifestVerifier) checkHTTP(ctx context.Context, env Environment, check Check) Result {
	result := Result{Check: check}
	instance := env.Instance()
	if instance == nil {
		return fail(result, "environment is not available")
	}
	endpoint := endpointFor(instance, check.Service)
	if endpoint == "" {
		return fail(result, fmt.Sprintf("service %q has no reachable endpoint", check.Service))
	}
	url := fmt.Sprintf("http://%s%s", endpoint, check.Path)
	method := check.Method
	if method == "" {
		method = "GET"
	}

	client := &http.Client{Timeout: v.HTTPTimeout}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return fail(result, err.Error())
	}
	resp, err := client.Do(req)
	if err != nil {
		return fail(result, fmt.Sprintf("request %s %s failed: %v", method, url, err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	result.Evidence = fmt.Sprintf("HTTP %d, %d bytes of body", resp.StatusCode, len(body))

	if check.Expect != nil && check.Expect.Status != nil {
		if resp.StatusCode != *check.Expect.Status {
			return fail(result, fmt.Sprintf("status %d, want %d", resp.StatusCode, *check.Expect.Status))
		}
	}
	if check.Expect != nil && check.Expect.Matches != nil {
		matched, err := regexp.MatchString(*check.Expect.Matches, string(body))
		if err != nil {
			return fail(result, fmt.Sprintf("bad pattern %q: %v", *check.Expect.Matches, err))
		}
		if !matched {
			return fail(result, fmt.Sprintf("body does not match %q", *check.Expect.Matches))
		}
	}
	result.Passed = true
	result.Detail = fmt.Sprintf("%s %s answered %d", method, check.Path, resp.StatusCode)
	return result
}

func (v *ManifestVerifier) checkMetric(ctx context.Context, env Environment, check Check, sampler *Sampler) Result {
	result := Result{Check: check}

	// When Agg is set, read from the sampler series collected during the
	// replay. This replaces the old checkMetricWindow which scraped for the
	// whole duration and always took max.
	if check.Agg != "" {
		if sampler == nil {
			return fail(result, "metric check with agg requires a sampler")
		}
		window := Window{}
		if check.Window != nil {
			window = *check.Window
		}
		value, ok, err := sampler.Aggregate(check.Query, Agg(check.Agg), window)
		if err != nil {
			return fail(result, fmt.Sprintf("metric aggregate: %v", err))
		}
		if !ok {
			return fail(result, fmt.Sprintf("metric %q not found in sampler series", check.Query))
		}
		result.Evidence = fmt.Sprintf("%s %s = %v", check.Query, check.Agg, value)

		if check.Expect == nil {
			return fail(result, "metric check requires expect.min or expect.max")
		}
		if check.Expect.Min != nil && value < *check.Expect.Min {
			return fail(result, fmt.Sprintf("%s %s = %v, want >= %v", check.Query, check.Agg, value, *check.Expect.Min))
		}
		if check.Expect.Max != nil && value > *check.Expect.Max {
			return fail(result, fmt.Sprintf("%s %s = %v, want <= %v", check.Query, check.Agg, value, *check.Expect.Max))
		}
		result.Passed = true
		result.Detail = fmt.Sprintf("%s %s = %v within bounds", check.Query, check.Agg, value)
		return result
	}

	samples, err := env.Runtime().Metrics(ctx, env.InstanceID(), check.Service)
	if err != nil {
		return fail(result, fmt.Sprintf("read metrics: %v", err))
	}
	var latest *runtime.MetricSample
	for i := range samples {
		if samples[i].Name != check.Query {
			continue
		}
		if latest == nil || samples[i].At.After(latest.At) {
			latest = &samples[i]
		}
	}
	if latest == nil {
		return fail(result, fmt.Sprintf("metric %q not found on service %q", check.Query, check.Service))
	}
	result.Evidence = fmt.Sprintf("%s = %v", latest.Name, latest.Value)

	if check.Expect == nil {
		return fail(result, "metric check requires expect.min or expect.max")
	}
	if check.Expect.Min != nil && latest.Value < *check.Expect.Min {
		return fail(result, fmt.Sprintf("%s = %v, want >= %v", latest.Name, latest.Value, *check.Expect.Min))
	}
	if check.Expect.Max != nil && latest.Value > *check.Expect.Max {
		return fail(result, fmt.Sprintf("%s = %v, want <= %v", latest.Name, latest.Value, *check.Expect.Max))
	}
	result.Passed = true
	result.Detail = fmt.Sprintf("%s = %v within bounds", latest.Name, latest.Value)
	return result
}

func (v *ManifestVerifier) checkLog(ctx context.Context, env Environment, check Check) Result {
	result := Result{Check: check}
	lines, err := env.Runtime().Logs(ctx, env.InstanceID(), runtime.LogOptions{
		Service: check.Service,
		Grep:    check.Pattern,
		Tail:    5000,
	})
	if err != nil {
		return fail(result, fmt.Sprintf("read logs: %v", err))
	}
	result.Evidence = fmt.Sprintf("%d matching lines", len(lines))
	if check.Absent {
		// Log absent: the check passes when NO line matches the pattern
		// (e.g. no secrets in logs).
		if len(lines) > 0 {
			return fail(result, fmt.Sprintf("found %d lines matching %q, expected none", len(lines), check.Pattern))
		}
		result.Passed = true
		result.Detail = fmt.Sprintf("no lines match %q (absent)", check.Pattern)
		return result
	}
	if len(lines) == 0 {
		return fail(result, fmt.Sprintf("no log line matches %q", check.Pattern))
	}
	result.Passed = true
	result.Detail = fmt.Sprintf("%d lines match %q", len(lines), check.Pattern)
	return result
}

func (v *ManifestVerifier) checkExec(ctx context.Context, env Environment, check Check) Result {
	result := Result{Check: check}
	if len(check.Command) == 0 {
		return fail(result, "exec check requires a command")
	}
	out, err := env.Runtime().Exec(ctx, env.InstanceID(), runtime.ExecRequest{
		Service: check.Service,
		Command: check.Command,
		Timeout: 30 * time.Second,
	})
	result.Evidence = strings.TrimSpace(out.Stdout + "\n" + out.Stderr)
	if err != nil {
		return fail(result, fmt.Sprintf("command failed: %v", err))
	}
	if check.Expect != nil && check.Expect.Equals != nil {
		if strings.TrimSpace(out.Stdout) != *check.Expect.Equals {
			return fail(result, fmt.Sprintf("output %q, want %q", strings.TrimSpace(out.Stdout), *check.Expect.Equals))
		}
	}
	if check.Expect != nil && check.Expect.Matches != nil {
		matched, err := regexp.MatchString(*check.Expect.Matches, out.Stdout)
		if err != nil {
			return fail(result, fmt.Sprintf("bad pattern: %v", err))
		}
		if !matched {
			return fail(result, fmt.Sprintf("output does not match %q", *check.Expect.Matches))
		}
	}
	if check.Expect != nil && check.Expect.Max != nil {
		value, convErr := strconv.ParseFloat(strings.TrimSpace(out.Stdout), 64)
		if convErr != nil {
			return fail(result, fmt.Sprintf("output %q is not a number", strings.TrimSpace(out.Stdout)))
		}
		if value > *check.Expect.Max {
			return fail(result, fmt.Sprintf("value %v, want <= %v", value, *check.Expect.Max))
		}
	}
	result.Passed = true
	result.Detail = "command succeeded"
	return result
}

func (v *ManifestVerifier) checkSQL(ctx context.Context, env Environment, check Check) Result {
	result := Result{Check: check}
	rows, err := env.SQL(ctx, check.Service, check.SQL)
	if err != nil {
		return fail(result, classifySQLError(err))
	}
	if len(rows) == 0 {
		return fail(result, "statement returned no rows")
	}
	result.Evidence = fmt.Sprintf("%v", rows)
	value, ok := firstNumeric(rows)
	if !ok {
		if check.Expect != nil && (check.Expect.Min != nil || check.Expect.Max != nil) {
			return fail(result, "statement returned a non-numeric value")
		}
		result.Passed = true
		result.Detail = "statement returned rows"
		return result
	}
	if check.Expect != nil && check.Expect.Min != nil && value < *check.Expect.Min {
		return fail(result, fmt.Sprintf("value %v, want >= %v", value, *check.Expect.Min))
	}
	if check.Expect != nil && check.Expect.Max != nil && value > *check.Expect.Max {
		return fail(result, fmt.Sprintf("value %v, want <= %v", value, *check.Expect.Max))
	}
	result.Passed = true
	result.Detail = fmt.Sprintf("sql value %v within bounds", value)
	return result
}

// checkUnit runs hidden unit tests for pure-code tasks. The test files live in
// scenarios/<taskID>/checks/*_test.go and are copied into a temporary directory
// together with the participant's workspace, then executed with `go test -race`
// in a throw-away container (--network none --read-only --memory --cpus
// --pids-limit, 60 s timeout). The participant's code on the host is never
// executed.
func (v *ManifestVerifier) checkUnit(ctx context.Context, env Environment, check Check, taskID string) Result {
	result := Result{Check: check}
	checksDir := filepath.Join(v.ProjectDir, "scenarios", taskID, "checks")
	testFiles, err := filepath.Glob(filepath.Join(checksDir, "*_test.go"))
	if err != nil {
		return fail(result, fmt.Sprintf("read checks dir %s: %v", checksDir, err))
	}
	if len(testFiles) == 0 {
		return fail(result, fmt.Sprintf("no *_test.go found in %s", checksDir))
	}

	// Copy workspace + test files into a temp dir.
	tmp, err := os.MkdirTemp("", "incidentsim-unit-*")
	if err != nil {
		return fail(result, fmt.Sprintf("create temp dir: %v", err))
	}
	defer os.RemoveAll(tmp)

	wsDir := env.WorkspaceDir()
	if wsDir == "" {
		return fail(result, "environment has no workspace dir")
	}
	if err := copyDir(wsDir, tmp); err != nil {
		return fail(result, fmt.Sprintf("copy workspace: %v", err))
	}
	for _, tf := range testFiles {
		data, err := os.ReadFile(tf)
		if err != nil {
			return fail(result, fmt.Sprintf("read test file %s: %v", tf, err))
		}
		if err := os.WriteFile(filepath.Join(tmp, filepath.Base(tf)), data, 0o644); err != nil {
			return fail(result, fmt.Sprintf("write test file %s: %v", tf, err))
		}
	}

	// Run go test -race in a restricted container.
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	args := []string{
		"run", "--rm", "--network", "none", "--read-only",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=128m",
		"--tmpfs", "/root/.cache:rw,noexec,nosuid,size=256m",
		"--memory", "512m", "--cpus", "1.0", "--pids-limit", "100",
		"-v", tmp + ":/src:ro", "-w", "/src",
		"golang:1.24-alpine", "go", "test", "-race", "./...",
	}
	cmd := exec.CommandContext(runCtx, "docker", args...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		result.Evidence = strings.TrimSpace(out.String() + "\n" + stderr.String())
		return fail(result, fmt.Sprintf("unit tests failed: %v", err))
	}
	result.Evidence = strings.TrimSpace(out.String())
	result.Passed = true
	result.Detail = "unit tests passed"
	return result
}

// copyDir recursively copies src into dst.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
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
}

// checkReplay grades a replay-based quality check. The field is one of
// error_rate|success_rate|p99_ms|rps|errors_reset, compared against expect.min
// or expect.max using the replay result from the single observation window.
func (v *ManifestVerifier) checkReplay(ctx context.Context, env Environment, check Check, replayResult ReplayResult) Result {
	result := Result{Check: check}
	if check.Field == "" {
		return fail(result, "replay check requires a field: error_rate|success_rate|p99_ms|rps|errors_reset")
	}
	var value float64
	switch check.Field {
	case "error_rate":
		total := replayResult.Requests + replayResult.Errors
		if total > 0 {
			value = float64(replayResult.Errors) / float64(total)
		}
	case "success_rate":
		total := replayResult.Requests + replayResult.Errors
		if total > 0 {
			value = float64(replayResult.Requests) / float64(total)
		}
	case "p99_ms":
		value = float64(replayResult.LatencyP99.Nanoseconds()) / 1e6
	case "rps":
		value = replayResult.RPS
	case "errors_reset":
		value = float64(replayResult.Errors)
	default:
		return fail(result, fmt.Sprintf("unknown replay field %q", check.Field))
	}
	result.Evidence = fmt.Sprintf("%s = %v", check.Field, value)

	if check.Expect == nil {
		return fail(result, "replay check requires expect.min or expect.max")
	}
	if check.Expect.Min != nil && value < *check.Expect.Min {
		return fail(result, fmt.Sprintf("%s = %v, want >= %v", check.Field, value, *check.Expect.Min))
	}
	if check.Expect.Max != nil && value > *check.Expect.Max {
		return fail(result, fmt.Sprintf("%s = %v, want <= %v", check.Field, value, *check.Expect.Max))
	}
	result.Passed = true
	result.Detail = fmt.Sprintf("%s = %v within bounds", check.Field, value)
	return result
}

// classifySQLError distinguishes a database connection problem from an
// invalid query, so the report tells the participant whether the database
// is unreachable or the statement is wrong.
func classifySQLError(err error) string {
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "connect: "),
		strings.Contains(lower, "no such host"):
		return "sql failed: database unreachable (connection refused)"
	case strings.Contains(lower, "deadline"),
		strings.Contains(lower, "timeout"),
		strings.Contains(lower, "i/o timeout"):
		return "sql failed: database unreachable (connection timeout)"
	case strings.Contains(lower, "syntax error"),
		strings.Contains(lower, "relation does not exist"),
		strings.Contains(lower, "column does not exist"):
		return fmt.Sprintf("sql failed: invalid query — %s", msg)
	default:
		return fmt.Sprintf("sql failed: %v", err)
	}
}

// endpointFor finds the reachable endpoint of a service on an instance.
func endpointFor(instance *runtime.Instance, service string) string {
	for _, declared := range instance.Services {
		if declared.Name == service && declared.Endpoint != "" {
			return declared.Endpoint
		}
	}
	return ""
}

func fail(result Result, detail string) Result {
	result.Passed = false
	result.Detail = detail
	return result
}

func firstNumeric(rows []map[string]string) (float64, bool) {
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		for _, value := range row {
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}
