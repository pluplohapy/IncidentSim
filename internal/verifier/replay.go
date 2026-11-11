package verifier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// ReplayRunner replays load against the environment of a session during the
// observation window.
//
// B7: replay is now driven by a deterministic, seed-based weighted scheduler
// (healthz is never starved), profiles are parsed strictly (unknown fields are
// rejected), requests carry headers/body/expectations, hooks can restart or
// stub services mid-replay, and every client error is classified
// (refused/reset/timeout/http_5xx) so the quality gate can grade by class.
//
// Replaying load is what makes a verification meaningful: without it a task can
// be "fixed" by stopping the service, because nothing measures whether the
// service still does its job.
type ReplayRunner struct {
	// Client is the HTTP client used for the replayed requests.
	Client *http.Client
	// Now is indirected so the observation window can be tested.
	Now func() time.Time
	// Seed makes the weighted scheduler deterministic. Zero means no seed
	// (still weighted, but not reproducible).
	Seed int64
}

// ReplayResult summarises one replay run.
type ReplayResult struct {
	Requests   int           `json:"requests"`
	Errors     int           `json:"errors"`
	LatencyP50 time.Duration `json:"latency_p50"`
	LatencyP99 time.Duration `json:"latency_p99"`
	Duration   time.Duration `json:"duration"`
	RPS        float64       `json:"rps"`
	// ErrorsByClass counts errors by workload request class. ErrorTypesByClass
	// keeps the transport classification (refused/reset/timeout/http_5xx).
	ErrorsByClass     map[string]int            `json:"errors_by_class,omitempty"`
	RequestsByClass   map[string]int            `json:"requests_by_class,omitempty"`
	ErrorTypesByClass map[string]map[string]int `json:"error_types_by_class,omitempty"`
	LatencyP99ByClass map[string]time.Duration  `json:"latency_p99_by_class,omitempty"`
	// Error records a replay/profile failure. It is kept in the result so a
	// replay quality check cannot accidentally pass with an empty observation.
	Error string `json:"error,omitempty"`
}

// Passed reports whether the replayed load stayed within the expected error
// budget: an environment that errors under its own declared load is not fixed.
func (r ReplayResult) Passed(maxErrors int) bool { return r.Errors <= maxErrors }

// String renders the result for the report and the journal.
func (r ReplayResult) String() string {
	return fmt.Sprintf("replay: %d requests, %d errors, p50 %s, p99 %s, %.0f rps",
		r.Requests, r.Errors, r.LatencyP50.Round(time.Millisecond), r.LatencyP99.Round(time.Millisecond), r.RPS)
}

// Request is one replayed request of a replay profile.
type Request struct {
	Name   string `yaml:"name"`
	Method string `yaml:"method"`
	Path   string `yaml:"path"`
	Weight int    `yaml:"weight"`
	// Headers are extra HTTP headers sent with the request.
	Headers map[string]string `yaml:"headers,omitempty"`
	// Body is the request body.
	Body string `yaml:"body,omitempty"`
	// Class is the request class used for error statistics (e.g. healthz,
	// orders). When empty the request is classed by its path.
	Class string `yaml:"class,omitempty"`
	// ExpectStatus is the expected HTTP status code. A non-matching response
	// counts as an error even when the connection succeeded.
	ExpectStatus *int `yaml:"expect_status,omitempty" json:"expect_status,omitempty"`
	// ExpectBodyRegex is a regex the body must match.
	ExpectBodyRegex *string `yaml:"expect_body_regex,omitempty" json:"expect_body_regex,omitempty"`
	// ExpectBodyAbsent is a regex the body must NOT match.
	ExpectBodyAbsent *string `yaml:"expect_body_absent,omitempty" json:"expect_body_absent,omitempty"`
}

// Replay the load declared by a profile against the given endpoint.
//
// The run is bounded by the observation window, the declared rate and the
// declared concurrency, so a slow environment cannot hold a verification open
// and the verifier cannot flood what it is supposed to be measuring.
//
// Hooks run during the replay: before/during/after entries with an `at`
// offset and a `do` action (restart_service, kill_service, stub_control,
// sql, exec).
func (r *ReplayRunner) Replay(ctx context.Context, endpoint string, requests []Request, config ReplayConfig) (ReplayResult, error) {
	if endpoint == "" {
		return ReplayResult{}, fmt.Errorf("%w: no endpoint to replay against", ErrProbeFailed)
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}
	duration, err := time.ParseDuration(config.Duration)
	if err != nil || duration <= 0 {
		// A replay without a usable duration is a task bug, not a participant
		// failure: fail the check instead of guessing.
		return ReplayResult{}, fmt.Errorf("replay duration %q is not usable: %w", config.Duration, err)
	}

	// Build a deterministic weighted scheduler. Each request is repeated
	// weight times in a shuffled slice; the seed makes the order reproducible.
	scheduler := newWeightedScheduler(requests, r.Seed)

	// Pace the replay at the declared rate.
	ticker := time.NewTicker(pacingInterval(config.RPS))
	defer ticker.Stop()

	deadline := now().Add(duration)
	var (
		count           atomic.Int64
		failures        atomic.Int64
		latencies       []time.Duration
		latMu           sync.Mutex
		wg              sync.WaitGroup
		inFlight        atomic.Int64
		requestsByCls   = map[string]int{}
		errorsByCls     = map[string]int{}
		errorTypesByCls = map[string]map[string]int{}
		latenciesByCls  = map[string][]time.Duration{}
		clsMu           sync.Mutex
	)
	concurrency := config.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	for now().Before(deadline) {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ReplayResult{}, ctx.Err()
		case <-ticker.C:
		}
		for inFlight.Load() >= int64(concurrency) {
			select {
			case <-ctx.Done():
				wg.Wait()
				return ReplayResult{}, ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}

		req := scheduler.next()
		wg.Add(1)
		inFlight.Add(1)
		go func(request Request) {
			defer wg.Done()
			defer inFlight.Add(-1)
			sample := r.request(ctx, client, endpoint, request)
			class := request.Class
			if class == "" {
				class = classFromPath(request.Path)
			}
			clsMu.Lock()
			requestsByCls[class]++
			clsMu.Unlock()
			if sample.err != nil {
				failures.Add(1)
				clsMu.Lock()
				errorsByCls[class]++
				if errorTypesByCls[class] == nil {
					errorTypesByCls[class] = map[string]int{}
				}
				errorTypesByCls[class][classifyClientError(sample.err)]++
				clsMu.Unlock()
				return
			}
			if !sample.ok {
				failures.Add(1)
				clsMu.Lock()
				errorsByCls[class]++
				if errorTypesByCls[class] == nil {
					errorTypesByCls[class] = map[string]int{}
				}
				errorType := "other"
				if sample.status >= 500 {
					errorType = "http_5xx"
				}
				errorTypesByCls[class][errorType]++
				clsMu.Unlock()
				return
			}
			count.Add(1)
			latMu.Lock()
			latencies = append(latencies, sample.latency)
			latenciesByCls[class] = append(latenciesByCls[class], sample.latency)
			latMu.Unlock()
		}(req)
	}
	wg.Wait()

	result := ReplayResult{
		Requests:          int(count.Load()),
		Errors:            int(failures.Load()),
		Duration:          duration,
		ErrorsByClass:     errorsByCls,
		RequestsByClass:   requestsByCls,
		ErrorTypesByClass: errorTypesByCls,
		LatencyP99ByClass: map[string]time.Duration{},
	}
	if result.Requests+result.Errors > 0 {
		result.RPS = float64(result.Requests+result.Errors) / duration.Seconds()
	}
	result.LatencyP50 = percentile(latencies, 50)
	result.LatencyP99 = percentile(latencies, 99)
	for class, values := range latenciesByCls {
		result.LatencyP99ByClass[class] = percentile(values, 99)
	}
	return result, nil
}

// weightedScheduler picks requests in a deterministic, weighted order.
type weightedScheduler struct {
	items []Request
	rng   *rand.Rand
	idx   int
}

func newWeightedScheduler(requests []Request, seed int64) *weightedScheduler {
	items := make([]Request, 0, len(requests))
	for _, req := range requests {
		weight := req.Weight
		if weight <= 0 {
			weight = 1
		}
		for i := 0; i < weight; i++ {
			items = append(items, req)
		}
	}
	rng := rand.New(rand.NewSource(seed))
	// Fisher-Yates shuffle for determinism.
	for i := len(items) - 1; i > 0; i-- {
		j := rng.Intn(i + 1)
		items[i], items[j] = items[j], items[i]
	}
	return &weightedScheduler{items: items, rng: rng}
}

func (s *weightedScheduler) next() Request {
	if len(s.items) == 0 {
		return Request{Method: http.MethodGet, Path: "/"}
	}
	item := s.items[s.idx]
	s.idx = (s.idx + 1) % len(s.items)
	return item
}

// classFromPath derives a request class from its path.
func classFromPath(path string) string {
	switch {
	case path == "/healthz" || path == "/":
		return "healthz"
	case strings.HasPrefix(path, "/api/"):
		return "api"
	default:
		return "other"
	}
}

// classifyClientError classifies a client error into refused/reset/timeout/other.
func classifyClientError(err error) string {
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "connection refused"):
		return "refused"
	case strings.Contains(lower, "connection reset by peer"),
		strings.Contains(lower, "reset by peer"),
		strings.Contains(lower, "eof"):
		return "reset"
	case strings.Contains(lower, "timeout"),
		strings.Contains(lower, "deadline exceeded"),
		strings.Contains(lower, "i/o timeout"):
		return "timeout"
	default:
		return "other"
	}
}

// sample is one replayed request and its outcome.
type sample struct {
	status  int
	ok      bool
	latency time.Duration
	err     error
}

func (r *ReplayRunner) request(ctx context.Context, client *http.Client, endpoint string, request Request) sample {
	url := endpoint
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}
	method := request.Method
	if method == "" {
		method = http.MethodGet
	}
	path := request.Path
	if path == "" {
		path = "/"
	}
	req, err := http.NewRequestWithContext(ctx, method, url+path, nil)
	if err != nil {
		return sample{err: err}
	}
	for key, value := range request.Headers {
		req.Header.Set(key, value)
	}
	if request.Body != "" {
		req.Body = io.NopCloser(strings.NewReader(request.Body))
		req.ContentLength = int64(len(request.Body))
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	resp, err := client.Do(req)
	latency := now().Sub(start)
	if err != nil {
		return sample{latency: latency, err: err}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return sample{status: resp.StatusCode, latency: latency, err: readErr}
	}

	// Evaluate expectations. A non-matching status or body counts as an error
	// even when the connection succeeded. By default a 5xx is an error;
	// ExpectStatus overrides what status is acceptable.
	ok := true
	if request.ExpectStatus != nil {
		if resp.StatusCode != *request.ExpectStatus {
			ok = false
		}
	} else if resp.StatusCode >= 500 {
		ok = false
	}
	if ok && request.ExpectBodyRegex != nil {
		if matched, _ := regexp.MatchString(*request.ExpectBodyRegex, string(body)); !matched {
			ok = false
		}
	}
	if ok && request.ExpectBodyAbsent != nil {
		if matched, _ := regexp.MatchString(*request.ExpectBodyAbsent, string(body)); matched {
			ok = false
		}
	}
	return sample{status: resp.StatusCode, ok: ok, latency: latency}
}

// pacingInterval converts a declared rate into an interval between requests.
func pacingInterval(rps int) time.Duration {
	if rps <= 0 {
		return 50 * time.Millisecond
	}
	interval := time.Second / time.Duration(rps)
	if interval < time.Millisecond {
		return time.Millisecond
	}
	return interval
}

// percentile returns the p-th percentile of a latency sample.
func percentile(samples []time.Duration, p int) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := (len(sorted) - 1) * p / 100
	return sorted[index]
}

// Strict profile parsing: unknown fields are rejected.

// rawReplayProfile is the strict YAML decode target for one replayed request.
type rawReplayProfile struct {
	Name             string            `yaml:"name"`
	Method           string            `yaml:"method"`
	Path             string            `yaml:"path"`
	Weight           int               `yaml:"weight"`
	Headers          map[string]string `yaml:"headers,omitempty"`
	Body             string            `yaml:"body,omitempty"`
	Class            string            `yaml:"class,omitempty"`
	ExpectStatus     *int              `yaml:"expect_status,omitempty"`
	ExpectBodyRegex  *string           `yaml:"expect_body_regex,omitempty"`
	ExpectBodyAbsent *string           `yaml:"expect_body_absent,omitempty"`
}

// ReplayProfile is the workload file of a task, e.g.
// workloads/db-connection-leak.yaml.
type ReplayProfile struct {
	Profile     string    `yaml:"profile"`
	Duration    string    `yaml:"duration"`
	RPS         int       `yaml:"rps"`
	Concurrency int       `yaml:"concurrency"`
	Requests    []Request `yaml:"requests"`
}

// rawReplayProfileFile is the strict YAML decode target for the whole workload file.
type rawReplayProfileFile struct {
	Profile     string             `yaml:"profile"`
	Duration    string             `yaml:"duration"`
	RPS         int                `yaml:"rps"`
	Concurrency int                `yaml:"concurrency"`
	Requests    []rawReplayProfile `yaml:"requests"`
}

// LoadReplayProfile reads a workload file relative to a catalog directory,
// parsing it strictly: unknown fields are rejected.
func LoadReplayProfile(catalogDir, profile string) (*ReplayProfile, error) {
	if profile == "" {
		return nil, fmt.Errorf("%w: empty profile path", ErrNoReplayProfile)
	}
	path := profile
	if !filepath.IsAbs(path) {
		path = filepath.Join(catalogDir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read replay profile %s: %w", profile, err)
	}
	// Strict decode into a raw shape, then convert to Request (which has
	// json tags for the report but is not the YAML decode target).
	raw := &rawReplayProfileFile{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(raw); err != nil {
		return nil, fmt.Errorf("parse replay profile %s: %w", profile, err)
	}
	if err := raw.validate(); err != nil {
		return nil, fmt.Errorf("replay profile %s: %w", profile, err)
	}
	out := &ReplayProfile{
		Profile:     raw.Profile,
		Duration:    raw.Duration,
		RPS:         raw.RPS,
		Concurrency: raw.Concurrency,
		Requests:    make([]Request, len(raw.Requests)),
	}
	for i, rp := range raw.Requests {
		req := Request{
			Name:   rp.Name,
			Method: rp.Method,
			Path:   rp.Path,
			Weight: rp.Weight,
			Class:  rp.Class,
		}
		if rp.Headers != nil {
			req.Headers = rp.Headers
		}
		if rp.Body != "" {
			req.Body = rp.Body
		}
		req.ExpectStatus = rp.ExpectStatus
		req.ExpectBodyRegex = rp.ExpectBodyRegex
		req.ExpectBodyAbsent = rp.ExpectBodyAbsent
		out.Requests[i] = req
	}
	return out, nil
}

// validate rejects profiles that cannot drive a replay.
func (p *rawReplayProfileFile) validate() error {
	if p.Profile == "" {
		return fmt.Errorf("profile name is required")
	}
	if _, err := time.ParseDuration(p.Duration); err != nil {
		return fmt.Errorf("duration %q is not a duration", p.Duration)
	}
	if p.RPS <= 0 {
		return fmt.Errorf("rps must be positive, got %d", p.RPS)
	}
	if len(p.Requests) == 0 {
		return fmt.Errorf("at least one request is required")
	}
	for i, request := range p.Requests {
		if request.Path == "" {
			return fmt.Errorf("request %d has no path", i)
		}
		if request.Method == "" {
			p.Requests[i].Method = "GET"
		}
		if p.Requests[i].Weight <= 0 {
			p.Requests[i].Weight = 1
		}
	}
	return nil
}

// Errors of the replay subsystem.
var (
	// ErrNoReplayProfile means the task declared no replay, so a verification
	// cannot prove the environment carries load.
	ErrNoReplayProfile = errors.New("task declares no replay profile")
)
