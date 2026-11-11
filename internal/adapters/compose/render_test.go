package compose

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"incidentsim/internal/runtime"
)

func testSpec() runtime.Spec {
	return runtime.Spec{
		Snapshot: "baseline",
		Services: []runtime.ServiceSpec{
			{
				Name:  "db",
				Image: "postgres:16-alpine",
				Env:   map[string]string{"POSTGRES_DB": "orders"},
				Ports: []int{5432},
			},
			{
				Name:  "app",
				Image: "incidentsim/workload:dev",
				Env: map[string]string{
					"DB_PORT":   "5432",
					"LOG_LEVEL": "info",
				},
				Ports:       []int{8080},
				Healthcheck: &runtime.HealthcheckSpec{Path: "/healthz", Port: 8080, IntervalSeconds: 5, TimeoutSeconds: 2},
			},
		},
	}
}

func TestProjectRendersServicesWithFaultEnvWinning(t *testing.T) {
	spec := testSpec()
	spec.FaultEnv = map[string]string{"LOG_LEVEL": "debug"}

	document := Project("incidentsim-test", "incidentsim-test", spec)

	for _, want := range []string{
		"name: incidentsim-test",
		"image: postgres:16-alpine",
		"image: incidentsim/workload:dev",
		"restart: unless-stopped",
		"healthcheck:",
		`- "5432"`,
		`- "8080"`,
	} {
		if !strings.Contains(document, want) {
			t.Fatalf("compose document missing %q:\n%s", want, document)
		}
	}
	// The fault must win over the declared environment, otherwise starting a
	// task would silently run a healthy system.
	app := section(document, "  app:")
	if !strings.Contains(app, `LOG_LEVEL: "debug"`) {
		t.Fatalf("fault env did not override service env:\n%s", app)
	}
	if strings.Contains(app, "LOG_LEVEL: info") {
		t.Fatalf("service env survived the fault:\n%s", app)
	}
}

func TestProjectQuotesNumericEnvValues(t *testing.T) {
	spec := testSpec()
	document := Project("incidentsim-test", "incidentsim-test", spec)
	if !strings.Contains(section(document, "  app:"), `DB_PORT: "5432"`) {
		t.Fatalf("numeric env value not quoted:\n%s", document)
	}
}

func TestProjectPublishesContainerPortsOnly(t *testing.T) {
	document := Project("incidentsim-test", "incidentsim-test", testSpec())
	// Fixed host ports would make two sessions of one task collide on a
	// developer machine, so the rendering must publish container ports only.
	if strings.Contains(document, ":5432") && strings.Contains(document, "5432:") {
		t.Fatalf("compose document still maps a host port:\n%s", document)
	}
	if strings.Count(document, `- "5432"`) != 1 || strings.Count(document, `- "8080"`) != 1 {
		t.Fatalf("unexpected port rendering:\n%s", document)
	}
}

func TestProjectRendersValidYAMLWithMountedConfig(t *testing.T) {
	document := Project("/state/incidentsim-test", "incidentsim-test", testSpec())

	// Compose rejects a project its YAML parser cannot read, and a rejected
	// project is a task that does not start at all — a failure that unit
	// tests of the individual renderers would never see. Parsing the whole
	// document here is what keeps that failure in the test suite.
	var parsed struct {
		Services map[string]struct {
			Volumes []string `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(document), &parsed); err != nil {
		t.Fatalf("compose document is not valid YAML: %v\n%s", err, document)
	}
	volumes := parsed.Services["app"].Volumes
	if len(volumes) != 1 {
		t.Fatalf("app volumes = %v, want exactly the mounted config file", volumes)
	}
	want := "/state/incidentsim-test/config/app.conf:/etc/app/app.conf:ro"
	if volumes[0] != want {
		t.Fatalf("volume = %q, want %q: the service must read the file it is graded on", volumes[0], want)
	}
}

func TestProjectNameIsDockerSafe(t *testing.T) {
	cases := map[string]string{
		"sess-123":  "incidentsim-sess-123",
		"Sess_123!": "incidentsim-sess-123-",
	}
	for in, want := range cases {
		if got := projectName(in); got != want {
			t.Errorf("projectName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostPortParsing(t *testing.T) {
	cases := map[string]int{
		"127.0.0.1:32768": 32768,
		"[::1]:49152":     49152,
		"0.0.0.0:8080":    8080,
	}
	for in, want := range cases {
		got, err := hostPort(in)
		if err != nil {
			t.Fatalf("hostPort(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("hostPort(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := hostPort("no-port"); err == nil {
		t.Error("hostPort accepted a malformed address")
	}
}

func TestParseLogLinesReadsComposePrefixes(t *testing.T) {
	text := `app-1  | 2026-01-01T10:00:00.123456789Z starting worker
db-1   | 2026-01-01T10:00:01.000000000Z ready to accept connections
app-1  | 2026-01-01T10:00:02.000000000Z GET /api/orders 200`

	all := parseLogLines(text, runtime.LogOptions{Service: "app"})
	if len(all) != 2 {
		t.Fatalf("got %d lines for service app, want 2: %+v", len(all), all)
	}
	if all[0].Service != "app" {
		t.Errorf("service = %q", all[0].Service)
	}
	if all[0].Text != "starting worker" {
		t.Errorf("text = %q", all[0].Text)
	}
	if all[0].Timestamp.IsZero() {
		t.Error("timestamp not parsed")
	}

	grep := parseLogLines(text, runtime.LogOptions{Service: "app", Grep: "GET"})
	if len(grep) != 1 || !strings.Contains(grep[0].Text, "/api/orders") {
		t.Fatalf("grep failed: %+v", grep)
	}

	tailed := parseLogLines(text, runtime.LogOptions{Service: "app", Tail: 1})
	if len(tailed) != 1 || tailed[0].Text != "GET /api/orders 200" {
		t.Fatalf("tail failed: %+v", tailed)
	}
}

func TestParseLogLinesWithoutTimestamps(t *testing.T) {
	lines := parseLogLines("plain line without prefix\n", runtime.LogOptions{Service: "app"})
	if len(lines) != 1 || lines[0].Text != "plain line without prefix" {
		t.Fatalf("plain line parsed as %+v", lines)
	}
}

func TestParsePrometheusLine(t *testing.T) {
	cases := []struct {
		line   string
		name   string
		value  float64
		wantOK bool
	}{
		{`app_errors_total 12`, "app_errors_total", 12, true},
		{`app_disk_used_pct{service="app"} 97.5`, "app_disk_used_pct", 97.5, true},
		{`# HELP app_errors_total errors`, "", 0, false},
		{`garbage`, "", 0, false},
	}
	for _, tc := range cases {
		name, labels, value, ok := parsePrometheusLine(tc.line)
		if ok != tc.wantOK {
			t.Fatalf("parsePrometheusLine(%q) ok = %v, want %v", tc.line, ok, tc.wantOK)
		}
		if !ok {
			continue
		}
		if name != tc.name || value != tc.value {
			t.Errorf("parsePrometheusLine(%q) = %s %v", tc.line, name, value)
		}
		if tc.name == "app_disk_used_pct" && labels["service"] != "app" {
			t.Errorf("labels = %v", labels)
		}
	}
}

// section returns the YAML block of one service, used to assert per-service
// rendering without a YAML dependency in the test.
func section(document, header string) string {
	lines := strings.Split(document, "\n")
	var (
		start = -1
		out   []string
	)
	for i, line := range lines {
		if line == header {
			start = i
			continue
		}
		if start >= 0 {
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
				break
			}
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
