package verifier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ReplayRunner replays load against the environment of a session during the
// observation window.
//
// Replaying load is what makes a verification meaningful: without it a task can
// be "fixed" by stopping the service, because nothing measures whether the
// service still does its job.
type ReplayRunner struct {
	// Client is the HTTP client used for the replayed requests.
	Client *http.Client
	// Now is indirected so the observation window can be tested.
	Now func() time.Time
}

// ReplayResult summarises one replay run.
type ReplayResult struct {
	Requests   int           `json:"requests"`
	Errors     int           `json:"errors"`
	LatencyP50 time.Duration `json:"latency_p50"`
	LatencyP99 time.Duration `json:"latency_p99"`
	Duration   time.Duration `json:"duration"`
	RPS        float64       `json:"rps"`
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
	Name   string
	Method string
	Path   string
	Weight int
}

// Replay the load declared by a profile against the given endpoint.
//
// The run is bounded by the observation window, the declared rate and the
// declared concurrency, so a slow environment cannot hold a verification open
// and the verifier cannot flood what it is supposed to be measuring.
func (r *ReplayRunner) Replay(ctx context.Context, endpoint string, requests []Request, config ReplayConfig) (ReplayResult, error) {
	if endpoint == "" {
		return ReplayResult{}, fmt.Errorf("%w: no endpoint to replay against", ErrProbeFailed)
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	duration, err := time.ParseDuration(config.Duration)
	if err != nil || duration <= 0 {
		// A replay without a usable duration is a task bug, not a participant
		// failure: fail the check instead of guessing.
		return ReplayResult{}, fmt.Errorf("replay duration %q is not usable: %w", config.Duration, err)
	}

	// Pace the replay at the declared rate: an unpaced replay measures how fast
	// the verifier can open sockets, not how the environment behaves.
	ticker := time.NewTicker(pacingInterval(config.RPS))
	defer ticker.Stop()

	deadline := r.Now().Add(duration)
	var (
		count     atomic.Int64
		failures  atomic.Int64
		latencies []time.Duration
		latMu     sync.Mutex
		wg        sync.WaitGroup
		inFlight  atomic.Int64
	)
	concurrency := config.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	for time.Now().Before(deadline) {
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

		wg.Add(1)
		inFlight.Add(1)
		go func() {
			defer wg.Done()
			defer inFlight.Add(-1)
			sample := r.request(ctx, client, endpoint, pickRequest(requests))
			if sample.err != nil {
				failures.Add(1)
				return
			}
			count.Add(1)
			if sample.status >= 500 {
				// A 5xx under replay is an error even when the connection
				// worked: the system is up but not correct.
				failures.Add(1)
			}
			latMu.Lock()
			latencies = append(latencies, sample.latency)
			latMu.Unlock()
		}()
	}
	wg.Wait()

	result := ReplayResult{
		Requests: int(count.Load()),
		Errors:   int(failures.Load()),
		Duration: duration,
	}
	if result.Requests+result.Errors > 0 {
		result.RPS = float64(result.Requests+result.Errors) / duration.Seconds()
	}
	result.LatencyP50 = percentile(latencies, 50)
	result.LatencyP99 = percentile(latencies, 99)
	return result, nil
}

// pickRequest chooses the next request of a profile by weight. A profile with
// one request degenerates into replaying that endpoint, which is the common
// case.
func pickRequest(requests []Request) Request {
	if len(requests) == 0 {
		return Request{Method: http.MethodGet, Path: "/"}
	}
	best := requests[0]
	bestWeight := 0
	for _, request := range requests {
		weight := request.Weight
		if weight <= 0 {
			weight = 1
		}
		if weight > bestWeight {
			best, bestWeight = request, weight
		}
	}
	return best
}

// sample is one replayed request and its outcome.
type sample struct {
	status  int
	latency time.Duration
	err     error
}

func (r *ReplayRunner) request(ctx context.Context, client *http.Client, endpoint string, request Request) sample {
	// A compose endpoint has no scheme, a microVM one has: normalise here so
	// the runner does not care which engine produced the environment.
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
	start := r.Now()
	resp, err := client.Do(req)
	latency := r.Now().Sub(start)
	if err != nil {
		return sample{latency: latency, err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return sample{status: resp.StatusCode, latency: latency}
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
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	index := (len(sorted) - 1) * p / 100
	return sorted[index]
}

// Errors of the replay subsystem.
var (
	// ErrNoReplayProfile means the task declared no replay, so a verification
	// cannot prove the environment carries load.
	ErrNoReplayProfile = errors.New("task declares no replay profile")
)
