// Package verifier grades a session on two levels.
//
// B6: Sampler replaces checkMetricWindow. During ONE replay the sampler
// scrapes /metrics every second into time series; quality checks compute an
// aggregate (max|min|avg|last|p95|slope|delta|resets) over a window
// {from: 5s} that skips the warmup. resets counts how many times a value
// decreased (a process restart). A single verify stays under 45 s.
package verifier

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Agg is the aggregate function a metric check applies to a sampled series.
type Agg string

const (
	AggMax    Agg = "max"
	AggMin    Agg = "min"
	AggAvg    Agg = "avg"
	AggLast   Agg = "last"
	AggP95    Agg = "p95"
	AggSlope  Agg = "slope"
	AggDelta  Agg = "delta"
	AggResets Agg = "resets"
)

// Series is one metric's observations during a replay.
type Series struct {
	Name   string
	Points []Sample
}

// Sample is one timestamped observation.
type Sample struct {
	At    time.Time
	Value float64
}

// Sampler scrapes /metrics of one service every second during a replay and
// keeps the series for later aggregate checks.
type Sampler struct {
	// HTTP client used to scrape.
	Client *http.Client
	// Now is indirected for tests.
	Now func() time.Time
	// interval between scrapes.
	Interval time.Duration
	// series holds the collected series, keyed by metric name.
	mu     sync.Mutex
	series map[string][]Sample
	stop   chan struct{}
	done   chan struct{}
}

// NewSampler returns a sampler that scrapes every second.
func NewSampler() *Sampler {
	return &Sampler{
		Client:   &http.Client{Timeout: 2 * time.Second},
		Now:      time.Now,
		Interval: time.Second,
		series:   map[string][]Sample{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins scraping the service endpoint every interval until Stop is
// called or ctx is cancelled. Safe to call once per Sampler instance.
func (s *Sampler) Start(ctx context.Context, endpoint, service string) {
	defer close(s.done)
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.scrape(endpoint, service)
		}
	}
}

// Stop halts scraping. Safe to call once per Sampler instance.
func (s *Sampler) Stop() {
	close(s.stop)
	<-s.done
}

// scrape performs one /metrics scrape and appends the samples.
func (s *Sampler) scrape(endpoint, service string) {
	url := endpoint
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}
	url += "/metrics"
	resp, err := s.Client.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	now := s.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, line := range strings.Split(string(body), "\n") {
		name, value, ok := parsePrometheusSample(line)
		if !ok {
			continue
		}
		s.series[name] = append(s.series[name], Sample{At: now, Value: value})
	}
}

// parsePrometheusSample parses one line of Prometheus text exposition format,
// e.g. `app_errors_total 3` or `app_disk_used_pct{service="app"} 97.5`. Lines
// starting with # are skipped.
func parsePrometheusSample(line string) (string, float64, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", 0, false
	}
	// Strip labels: everything up to the first space after an optional {...}.
	valueStart := strings.IndexByte(line, ' ')
	if valueStart < 0 {
		return "", 0, false
	}
	nameAndLabels := line[:valueStart]
	valueStr := strings.TrimSpace(line[valueStart:])
	// Drop labels: name is everything before the first '{'.
	name := nameAndLabels
	if idx := strings.IndexByte(nameAndLabels, '{'); idx >= 0 {
		name = nameAndLabels[:idx]
	}
	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return "", 0, false
	}
	return name, value, true
}

// Series returns a copy of the series for one metric.
func (s *Sampler) Series(name string) []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	pts := make([]Sample, len(s.series[name]))
	copy(pts, s.series[name])
	return pts
}

// All returns a copy of every series.
func (s *Sampler) All() map[string][]Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]Sample, len(s.series))
	for name, pts := range s.series {
		cp := make([]Sample, len(pts))
		copy(cp, pts)
		out[name] = cp
	}
	return out
}

// Aggregate computes the aggregate of a series over a window.
// window.From skips the leading warmup points.
func (s *Sampler) Aggregate(name string, agg Agg, window Window) (float64, bool, error) {
	pts := s.Series(name)
	if len(pts) == 0 {
		return 0, false, nil
	}
	// Apply warmup skip.
	start := 0
	if window.From != "" {
		d, err := time.ParseDuration(window.From)
		if err == nil {
			cutoff := s.Now().Add(-d)
			for i := len(pts) - 1; i >= 0; i-- {
				if pts[i].At.Before(cutoff) {
					start = i + 1
					break
				}
			}
		}
	}
	pts = pts[start:]
	if len(pts) == 0 {
		return 0, false, nil
	}
	switch agg {
	case AggMax:
		return maxOf(pts), true, nil
	case AggMin:
		return minOf(pts), true, nil
	case AggAvg:
		return avgOf(pts), true, nil
	case AggLast:
		return pts[len(pts)-1].Value, true, nil
	case AggP95:
		return percentileOf(pts, 95), true, nil
	case AggSlope:
		return slopeOf(pts), true, nil
	case AggDelta:
		return deltaOf(pts), true, nil
	case AggResets:
		return float64(resetsOf(pts)), true, nil
	default:
		return 0, false, fmt.Errorf("unknown agg %q", agg)
	}
}

func maxOf(pts []Sample) float64 {
	m := pts[0].Value
	for _, p := range pts[1:] {
		if p.Value > m {
			m = p.Value
		}
	}
	return m
}

func minOf(pts []Sample) float64 {
	m := pts[0].Value
	for _, p := range pts[1:] {
		if p.Value < m {
			m = p.Value
		}
	}
	return m
}

func avgOf(pts []Sample) float64 {
	var sum float64
	for _, p := range pts {
		sum += p.Value
	}
	return sum / float64(len(pts))
}

func percentileOf(pts []Sample, p int) float64 {
	vals := make([]float64, len(pts))
	for i, p := range pts {
		vals[i] = p.Value
	}
	sort.Float64s(vals)
	if len(vals) == 0 {
		return 0
	}
	index := (len(vals) - 1) * p / 100
	return vals[index]
}

func slopeOf(pts []Sample) float64 {
	if len(pts) < 2 {
		return 0
	}
	first := pts[0]
	last := pts[len(pts)-1]
	dt := last.At.Sub(first.At).Seconds()
	if dt == 0 {
		return 0
	}
	return (last.Value - first.Value) / dt
}

func deltaOf(pts []Sample) float64 {
	if len(pts) < 2 {
		return 0
	}
	return pts[len(pts)-1].Value - pts[0].Value
}

func resetsOf(pts []Sample) int {
	resets := 0
	for i := 1; i < len(pts); i++ {
		if pts[i].Value < pts[i-1].Value {
			resets++
		}
	}
	return resets
}
