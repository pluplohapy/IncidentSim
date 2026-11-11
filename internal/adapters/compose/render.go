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
// Ports are published without a fixed host port: Docker assigns an ephemeral
// one, so two sessions of the same task never collide on a developer machine
// and a restarted session does not inherit a stale binding. The adapter then
// asks `compose port` for the real mapping, which is what makes the endpoints
// it reports honest.
func Project(projectDir, name string, spec runtime.Spec) string {
	var b strings.Builder
	b.WriteString("name: ")
	b.WriteString(name)
	b.WriteString("\nservices:\n")

	for _, service := range spec.Services {
		writeService(&b, projectDir, service, spec.FaultEnv, spec.CPUCount, spec.MemoryMiB)
	}

	b.WriteString("networks:\n  default:\n    name: ")
	b.WriteString(name)
	b.WriteString("\n")
	return b.String()
}

func writeService(b *strings.Builder, projectDir string, service runtime.ServiceSpec, faultEnv map[string]string, cpuCount, memoryMiB int) {
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
			// Publish on loopback only: two sessions must not collide, and the
			// host must not expose task ports to the network.
			b.WriteString(fmt.Sprintf("      - \"127.0.0.1::%d\"\n", port))
		}
	}

	// The generated configuration file is mounted into the container. It is
	// read-only on purpose: the file the service loads is the host's file, so
	// a write inside the container would change what a verification grades
	// without going through Apply (which records it, validates it against
	// spec.editable and re-creates the service). A shell inside the container
	// can read it, which is what debugging needs, and cannot silently rewrite
	// the artefact it is graded on.
	b.WriteString("    volumes:\n")
	b.WriteString(fmt.Sprintf("      - %q\n",
		fmt.Sprintf("%s:%s/%s:ro",
			configPath(projectDir, service.Name), configMountDir, service.Name+".conf")))

	if service.Healthcheck != nil {
		interval := service.Healthcheck.IntervalSeconds
		if interval <= 0 {
			interval = 5
		}
		timeout := service.Healthcheck.TimeoutSeconds
		if timeout <= 0 {
			timeout = 3
		}
		b.WriteString(fmt.Sprintf("    healthcheck:\n      test: [\"CMD-SHELL\", \"wget -q --spider http://127.0.0.1:%d%s || exit 1\"]\n      interval: %ds\n      timeout: %ds\n      retries: 10\n",
			service.Healthcheck.Port, service.Healthcheck.Path, interval, timeout))
	}

	// Resource limits isolate the task environment from the host and from
	// other sessions. CPU is expressed as a fraction of a core (0.25 = one
	// quarter), memory as bytes.
	if cpuCount > 0 || memoryMiB > 0 {
		b.WriteString("    deploy:\n")
		b.WriteString("      resources:\n")
		b.WriteString("        limits:\n")
		if cpuCount > 0 {
			b.WriteString(fmt.Sprintf("          cpus: '%.2f'\n", float64(cpuCount)))
		}
		if memoryMiB > 0 {
			b.WriteString(fmt.Sprintf("          memory: %dM\n", memoryMiB))
		}
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
