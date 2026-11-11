package compose

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"incidentsim/internal/runtime"
)

// scrapeMetrics reads a Prometheus text exposition from a service endpoint.
//
// Prometheus is the metrics format of the whole system. The service exposes it
// directly, so the core parses metrics in one place.
func scrapeMetrics(ctx context.Context, url, service string) ([]runtime.MetricSample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape %s: status %d", url, resp.StatusCode)
	}

	var samples []runtime.MetricSample
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, ok := parsePrometheusLine(line)
		if !ok {
			continue
		}
		samples = append(samples, runtime.MetricSample{
			Service: service,
			Name:    name,
			Value:   value,
			Labels:  labels,
			At:      time.Now(),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scrape %s: %w", url, err)
	}
	return samples, nil
}

// parsePrometheusLine parses `metric{label="value"} 12.5`.
func parsePrometheusLine(line string) (name string, labels map[string]string, value float64, ok bool) {
	rest := line
	if brace := strings.IndexByte(rest, '{'); brace >= 0 {
		name = strings.TrimSpace(rest[:brace])
		end := strings.LastIndexByte(rest, '}')
		if end < brace {
			return "", nil, 0, false
		}
		labels = parseLabels(rest[brace+1 : end])
		rest = strings.TrimSpace(rest[end+1:])
	} else if space := strings.LastIndexAny(rest, " \t"); space >= 0 {
		name = strings.TrimSpace(rest[:space])
		rest = strings.TrimSpace(rest[space+1:])
	} else {
		return "", nil, 0, false
	}
	if name == "" {
		return "", nil, 0, false
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	if err != nil {
		return "", nil, 0, false
	}
	return name, labels, parsed, true
}

func parseLabels(text string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(text, ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return out
}
