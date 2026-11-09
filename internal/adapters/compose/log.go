package compose

import (
	"strconv"
	"strings"
	"time"

	"incidentsim/internal/runtime"
)

// parseLogLines turns `docker compose logs --timestamps` output into log lines.
//
// Compose prefixes every line with the service name and, with --timestamps, an
// RFC3339 timestamp: "app-1  | 2026-01-01T10:00:00.000000000Z line text".
func parseLogLines(text string, opts runtime.LogOptions) []runtime.LogLine {
	var out []runtime.LogLine
	for _, raw := range strings.Split(text, "\n") {
		if raw == "" {
			continue
		}
		service, timestamp, body := splitPrefixed(raw)
		if opts.Service != "" && service != "" && service != opts.Service {
			continue
		}
		if opts.Grep != "" && !strings.Contains(body, opts.Grep) {
			continue
		}
		out = append(out, runtime.LogLine{
			Service:   firstNonEmpty(service, opts.Service),
			Timestamp: timestamp,
			Stream:    "stdout",
			Text:      body,
		})
	}
	if opts.Tail > 0 && len(out) > opts.Tail {
		out = out[len(out)-opts.Tail:]
	}
	return out
}

// splitPrefixed pulls the service prefix and the timestamp off a log line.
// Compose v2 prefixes with "service-1  | ", so the instance suffix is stripped
// to recover the manifest service name.
func splitPrefixed(line string) (service string, timestamp time.Time, body string) {
	rest := line
	if pipe := strings.Index(rest, "|"); pipe >= 0 {
		service = strings.TrimSpace(rest[:pipe])
		service = stripInstanceSuffix(service)
		rest = strings.TrimSpace(rest[pipe+1:])
	}
	if space := strings.IndexAny(rest, " \t"); space > 0 {
		if parsed, err := time.Parse(time.RFC3339Nano, rest[:space]); err == nil {
			timestamp = parsed
			rest = strings.TrimSpace(rest[space+1:])
		}
	}
	return service, timestamp, rest
}

// stripInstanceSuffix turns "app-1" into "app".
func stripInstanceSuffix(prefix string) string {
	if idx := strings.LastIndex(prefix, "-"); idx > 0 {
		if _, err := strconv.Atoi(prefix[idx+1:]); err == nil {
			return prefix[:idx]
		}
	}
	return prefix
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
