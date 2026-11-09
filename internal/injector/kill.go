package injector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"incidentsim/internal/runtime"
)

// KillInjector injects an availability fault by stopping a service.
//
// It is the injector behind the "process died" incident: a worker that stops,
// a queue consumer that is not running. The restart it performs afterwards is
// a real restart of the declared process, not a compose-level recreate, so the
// symptom is a stopped process and not a removed container.
type KillInjector struct {
	// RestartDelay is how long the service stays down before it comes back.
	// A fault that never heals is indistinguishable from a broken task.
	RestartDelay time.Duration
}

// NewKillInjector returns the process-level fault injector.
func NewKillInjector() *KillInjector {
	return &KillInjector{RestartDelay: 5 * time.Second}
}

// Kind implements Injector.
func (i *KillInjector) Kind() string { return KindKill }

// Activate implements Injector.
func (i *KillInjector) Activate(ctx context.Context, env Environment, fault Fault) error {
	options := NewOptions(fault.Options)
	process := options.String("process", "")
	signal := options.String("signal", "TERM")

	result, err := env.Runtime().Exec(ctx, env.InstanceID(), runtime.ExecRequest{
		Service: fault.Target,
		Command: []string{"sh", "-c", killCommand(process, signal)},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("kill on %s: %w", fault.Target, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("kill on %s exited %d: %s", fault.Target, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// killCommand builds the shell command that stops the process. A pattern is
// matched rather than a pid, because inside a container the supervisor does not
// expose the pid the participant would look for.
func killCommand(process, signal string) string {
	if process == "" {
		return fmt.Sprintf("kill -%s 1", signal)
	}
	return fmt.Sprintf("pkill -%s -f %q", signal, process)
}
