package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"incidentsim/internal/runtime"
)

// writeServiceConfig materialises the environment of every service as a
// configuration file in the compose project directory, and mounts it into the
// container.
//
// The file is the same one the service reads at startup, so an edit followed by
// a restart is a real fix: the change is where the participant made it, it
// survives a restart, and the verifier reads the same file rather than a
// separate API-bookkeeping copy.
func writeServiceConfig(projectDir string, spec runtime.Spec) error {
	configDir := filepath.Join(projectDir, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	for _, service := range spec.Services {
		path := filepath.Join(configDir, service.Name+".conf")
		if err := os.WriteFile(path, []byte(renderConfig(service.Env, spec.FaultEnv)), 0o600); err != nil {
			return fmt.Errorf("write config for %s: %w", service.Name, err)
		}
	}
	return nil
}

// renderConfig renders a configuration file in KEY=VALUE form, one per line.
func renderConfig(env, faultEnv map[string]string) string {
	merged := make(map[string]string, len(env)+len(faultEnv))
	for key, value := range env {
		merged[key] = value
	}
	for key, value := range faultEnv {
		merged[key] = value
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# IncidentSim service configuration.\n")
	b.WriteString("# Values are read at startup; editing this file and restarting\n")
	b.WriteString("# the service is a real fix.\n")
	for _, key := range keys {
		b.WriteString(fmt.Sprintf("%s=%s\n", key, quoteConfigValue(merged[key])))
	}
	return b.String()
}

// quoteConfigValue keeps values that would otherwise be read as comments from
// breaking the file.
func quoteConfigValue(value string) string {
	if strings.HasPrefix(value, "#") {
		return fmt.Sprintf("%q", value)
	}
	return value
}

// configPath returns the path of the generated configuration file inside the
// compose project directory.
func configPath(projectDir, service string) string {
	return filepath.Join(projectDir, "config", service+".conf")
}

// configMountDir is where the generated configuration file is mounted inside
// each service container. A task service reads this file at startup, so
// editing it and restarting the service is a real fix.
const configMountDir = "/etc/app"
