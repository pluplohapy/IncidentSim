package verifier

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
type ManifestVerifier struct {
	// HTTPTimeout bounds every http check.
	HTTPTimeout time.Duration
	// ReplayRunner replays load before the correctness checks run. It is a
	// field so a test can substitute a runner with a controlled clock.
	ReplayRunner *ReplayRunner
	// ProjectDir is the repository root the profiles are relative to, so a
	// manifest writes workloads/db-connection-leak.yaml exactly as the path
	// looks on disk. A task whose profile cannot be loaded fails loudly.
	ProjectDir string
}

// NewManifestVerifier returns the default grader with production timeouts.
func NewManifestVerifier() *ManifestVerifier {
	return &ManifestVerifier{
		HTTPTimeout:  15 * time.Second,
		ReplayRunner: &ReplayRunner{Now: time.Now},
	}
}

// Verify implements Verifier.
func (v *ManifestVerifier) Verify(ctx context.Context, env Environment, sessionID, taskID string, checks CheckSet) (Report, error) {
	report := NewReport(sessionID, taskID)
	if env == nil {
		return report, fmt.Errorf("%w: no environment given", ErrProbeFailed)
	}

	// The observation window runs first: a gate that measures an idle system
	// would let a task be "fixed" by stopping the service.
	report.Replay = v.runReplay(ctx, env, checks)
	report.Correctness.Results = v.runGate(ctx, env, checks.Correctness)
	report.Quality.Results = v.runGate(ctx, env, checks.Quality)
	report.Finalise()
	return report, nil
}

// runReplay replays the declared load and returns a summary for the report.
// A missing profile is a note, not a failure: a static task may have nothing to
// replay, and the gates still run against the current state.
//
// The replay targets the service the correctness checks measure. Picking the
// first service of the environment instead would replay against a database,
// which speaks no HTTP and reports a thousand meaningless errors.
func (v *ManifestVerifier) runReplay(ctx context.Context, env Environment, checks CheckSet) string {
	config := checks.Replay
	if config == nil || config.Duration == "" {
		return ""
	}
	instance := env.Instance()
	if instance == nil {
		return "replay skipped: no environment"
	}
	requests, err := v.profileRequests(config)
	if err != nil {
		return fmt.Sprintf("replay skipped: %v", err)
	}
	target := replayTarget(instance, checks)
	if target == "" {
		return "replay skipped: no HTTP service in the environment"
	}
	result, err := v.ReplayRunner.Replay(ctx, target, requests, *config)
	if err != nil {
		return fmt.Sprintf("replay incomplete: %v", err)
	}
	return result.String()
}

// replayTarget resolves the endpoint the replay should load.
func replayTarget(instance *runtime.Instance, checks CheckSet) string {
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
	return profile.Requests, nil
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
		result = v.checkMetric(ctx, env, check)
	case CheckLog:
		result = v.checkLog(ctx, env, check)
	case CheckExec:
		result = v.checkExec(ctx, env, check)
	case CheckSQL:
		result = v.checkSQL(ctx, env, check)
	case CheckPolicy:
		result = v.checkPolicy(ctx, env, check)
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

func (v *ManifestVerifier) checkMetric(ctx context.Context, env Environment, check Check) Result {
	result := Result{Check: check}
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
		return fail(result, fmt.Sprintf("sql failed: %v", err))
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
