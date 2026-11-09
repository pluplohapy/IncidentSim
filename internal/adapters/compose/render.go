package compose

import (
	"fmt"
	"sort"
	"strings"

	"incidentsim/internal/runtime"
)

// composeFile is the generated project file name. It is fixed so the CLI
// invocation does not need to vary per session.
const composeFile = "compose.yaml"

// Project renders a runtime.Spec into a compose project document.
//
// Port mapping is deterministic: the first host port is 10000 + index, so two
// sessions of the same task never collide on a developer machine. The core
// learns the mapping back through `compose port`, which is what makes the
// Service endpoints honest even if Docker chose a different host port.
func Project(name string, spec runtime.Spec) string {
	var b strings.Builder
	b.WriteString("name: ")
	b.WriteString(name)
	b.WriteString("\nservices:\n")

	hostPort := 20000
	for i, service := range spec.Services {
		hostPort = 20000 + i*100
		writeService(&b, service, hostPort, spec.FaultEnv)
	}

	b.WriteString("networks:\n  default:\n    name: ")
	b.WriteString(name)
	b.WriteString("\n")
	return b.String()
}

func writeService(b *strings.Builder, service runtime.ServiceSpec, hostPort int, faultEnv map[string]string) {
	b.WriteString("  ")
	b.WriteString(service.Name)
	b.WriteString(":\n")
	b.WriteString("    image: ")
	b.WriteString(service.Image)
	b.WriteString("\n")
	b.WriteString("    restart: unless-stopped\n")

	if len(service.Command) > 0 {
		b.WriteString("    command: ")
		b.WriteString(yamlList(service.Command))
		b.WriteString("\n")
	}

	env := mergedEnv(service.Env, faultEnv)
	if len(env) > 0 {
		b.WriteString("    environment:\n")
		for _, key := range sortedKeys(env) {
			b.WriteString("      ")
			b.WriteString(key)
			b.WriteString(": ")
			b.WriteString(quote(env[key]))
			b.WriteString("\n")
		}
	}

	if len(service.Ports) > 0 {
		b.WriteString("    ports:\n")
		for _, port := range service.Ports {
			b.WriteString(fmt.Sprintf("      - \"%d:%d\"\n", hostPort, port))
		}
	}

	if service.Healthcheck != nil {
		interval := service.Healthcheck.IntervalSeconds
		if interval <= 0 {
			interval = 5
		}
		timeout := service.Healthcheck.TimeoutSeconds
		if timeout <= 0 {
			timeout = 3
		}
		b.WriteString(fmt.Sprintf("    healthcheck:\n      test: [\"CMD-SHELL\", \"curl -fsS http://127.0.0.1:%d%s || exit 1\"]\n      interval: %ds\n      timeout: %ds\n      retries: 10\n",
			service.Healthcheck.Port, service.Healthcheck.Path, interval, timeout))
	}
}

// mergedEnv combines the service environment with the fault environment. The
// fault wins: that is the whole point of an injected fault.
func mergedEnv(serviceEnv, faultEnv map[string]string) map[string]string {
	out := make(map[string]string, len(serviceEnv)+len(faultEnv))
	for key, value := range serviceEnv {
		out[key] = value
	}
	for key, value := range faultEnv {
		out[key] = value
	}
	return out
}

func sortedKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func yamlList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, quote(item))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// quote always quotes an environment value. Quoting unconditionally is
// deliberate: it stops YAML from reinterpreting values such as DB_PORT "5432"
// or a flag literally named "no" as a number or a boolean.
func quote(value string) string {
	return fmt.Sprintf("%q", value)
}
