package verifier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"incidentsim/internal/runtime"
)

const (
	RuleEnvFlagOff    = "env_flag_off"
	RuleEnvFlagEquals = "env_flag_equals"
	// RuleConfigFlagEquals is the file-based twin of env_flag_equals: it
	// reads the configuration file the service loads at startup, which is
	// the only place a config-level fix actually lives.
	RuleConfigFlagEquals   = "config_flag_equals"
	RuleServiceNotStopped  = "service_not_stopped"
	RuleLogOutputPreserved = "log_output_preserved"
	RuleLoggingNotDisabled = "logging_not_disabled"
	RuleEnvIntAtLeast      = "env_int_at_least"
	RuleRetryLoopBounded   = "retry_loop_bounded"

	// defaultRetryAttemptsCap is the amplification budget a bounded retry
	// policy stays under when the check does not name one: above it the
	// retries multiply the load instead of masking a transient failure.
	defaultRetryAttemptsCap = 5
)

func (v *ManifestVerifier) checkPolicy(ctx context.Context, env Environment, check Check) Result {
	result := Result{Check: check}
	rule, ok := PolicyRules[check.Rule]
	if !ok {
		return fail(result, fmt.Sprintf("unknown policy rule %q (have %s)", check.Rule, ruleNames()))
	}
	var vars map[string]string
	var err error
	if check.File != "" {
		// A check that names a file reads the file the service loads at
		// startup. The environment of the process is a different artefact:
		// a compose service keeps its declared variables across a config
		// edit, so an env-based check would grade the noise, not the fix.
		vars, err = readConfigFile(ctx, env, check.Service, check.File)
		result.Evidence = fmt.Sprintf("file %s: %s=%q", check.File, check.Query, vars[check.Query])
	} else {
		vars, err = env.Env(ctx, check.Service)
		result.Evidence = fmt.Sprintf("%s=%q", check.Query, vars[check.Query])
	}
	if err != nil {
		return fail(result, fmt.Sprintf("read config for %q: %v", check.Service, err))
	}
	if err := rule(ctx, env, check, vars); err != nil {
		return fail(result, err.Error())
	}
	result.Passed = true
	result.Detail = fmt.Sprintf("policy %s satisfied", check.Rule)
	return result
}

// readConfigFile reads a KEY=VALUE configuration file inside a service
// container. It is the real read path of a config-level fix: the file on
// disk, not a bookkeeping copy the core keeps for itself.
func readConfigFile(ctx context.Context, env Environment, service, path string) (map[string]string, error) {
	out, err := env.Runtime().Exec(ctx, env.InstanceID(), runtime.ExecRequest{
		Service: service,
		Command: []string{"cat", path},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("read %s: exit %d: %s", path, out.ExitCode, strings.TrimSpace(out.Stderr))
	}
	vars := map[string]string{}
	for _, line := range strings.Split(out.Stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		vars[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return vars, nil
}

var PolicyRules = map[string]func(ctx context.Context, env Environment, check Check, vars map[string]string) error{
	RuleEnvFlagOff: func(ctx context.Context, env Environment, check Check, vars map[string]string) error {
		value, present := vars[check.Query]
		if !present || strings.TrimSpace(value) == "" || isOff(value) {
			return nil
		}
		return fmt.Errorf("%s is still %q, expected the flag to be off", check.Query, value)
	},
	RuleEnvFlagEquals: func(ctx context.Context, env Environment, check Check, vars map[string]string) error {
		value, present := vars[check.Query]
		want := ""
		if check.Expect != nil && check.Expect.Equals != nil {
			want = *check.Expect.Equals
		}
		if !present {
			return fmt.Errorf("%s is not set, expected %q", check.Query, want)
		}
		if value != want {
			return fmt.Errorf("%s is %q, want %q", check.Query, value, want)
		}
		return nil
	},
	RuleConfigFlagEquals: func(ctx context.Context, env Environment, check Check, vars map[string]string) error {
		value, present := vars[check.Query]
		want := ""
		if check.Expect != nil && check.Expect.Equals != nil {
			want = *check.Expect.Equals
		}
		if !present {
			return fmt.Errorf("%s is not set in %s, expected %q", check.Query, check.File, want)
		}
		if value != want {
			return fmt.Errorf("%s is %q in %s, want %q", check.Query, value, check.File, want)
		}
		return nil
	},
	RuleServiceNotStopped: func(ctx context.Context, env Environment, check Check, vars map[string]string) error {
		if len(vars) == 0 {
			return fmt.Errorf("service %q has no environment: it is not running", check.Service)
		}
		return nil
	},
	RuleLogOutputPreserved: func(ctx context.Context, env Environment, check Check, vars map[string]string) error {
		path := vars[check.Query]
		if path == "" {
			return fmt.Errorf("%q is not set: the service does not know where it logs", check.Query)
		}
		// Check inside the container first: LOG_FILE is a container path,
		// not a host path.
		result, err := env.Runtime().Exec(ctx, env.InstanceID(), runtime.ExecRequest{
			Service: check.Service,
			Command: []string{"sh", "-c", fmt.Sprintf("test -s %q", path)},
			Timeout: 10 * time.Second,
		})
		if err != nil {
			if errors.Is(err, runtime.ErrUnsupported) {
				// Fall back to host stat for a runtime that cannot exec inside
				// the container.
				info, statErr := os.Stat(path)
				if statErr != nil {
					return fmt.Errorf("log file %s is gone: %v", path, statErr)
				}
				if info.Size() == 0 {
					return fmt.Errorf("log file %s is empty: the history was destroyed", path)
				}
				return nil
			}
			return fmt.Errorf("log file %s check failed: %v", path, err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("log file %s is missing or empty: the history was destroyed", path)
		}
		return nil
	},
	RuleEnvIntAtLeast: func(ctx context.Context, env Environment, check Check, vars map[string]string) error {
		value, present := vars[check.Query]
		want := 0
		if check.Expect != nil && check.Expect.Min != nil {
			want = int(*check.Expect.Min)
		}
		got, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || !present {
			return fmt.Errorf("%s is %q, expected a number >= %v", check.Query, value, want)
		}
		if got < want {
			return fmt.Errorf("%s is %v, want >= %v", check.Query, got, want)
		}
		return nil
	},
	// RuleRetryLoopBounded requires the retry policy of a service to keep
	// retrying and to back off. The chain-aware check: the amplifying link of
	// a fault chain is bounded rather than removed, because removing retries
	// turns a storm into a fast failure and only hides the loop.
	//
	// A policy that names only the backoff is a correct fix, so the rule must
	// not demand an explicit RETRY_ENABLED: the flag defaults to on, and only
	// a value that says off is a removal of the loop's resilience. What it
	// does bound is the amplification itself, so the attempt budget is part
	// of the same rule.
	RuleRetryLoopBounded: func(ctx context.Context, env Environment, check Check, vars map[string]string) error {
		if enabled, present := vars["RETRY_ENABLED"]; present && isOff(enabled) {
			return fmt.Errorf("retry is disabled: the storm is gone and so is the resilience that depended on it")
		}
		backoff, err := strconv.Atoi(strings.TrimSpace(vars["RETRY_BACKOFF_MS"]))
		want := 0
		if check.Expect != nil && check.Expect.Min != nil {
			want = int(*check.Expect.Min)
		}
		if err != nil || backoff < want {
			return fmt.Errorf("RETRY_BACKOFF_MS is %q, want >= %d ms: without a backoff the storm re-forms", vars["RETRY_BACKOFF_MS"], want)
		}
		// An absent budget means the service default, which is bounded; only a
		// budget that names too many attempts is the amplification.
		attempts, err := strconv.Atoi(strings.TrimSpace(vars["RETRY_ATTEMPTS"]))
		cap := defaultRetryAttemptsCap
		if check.Expect != nil && check.Expect.Max != nil {
			cap = int(*check.Expect.Max)
		}
		if err == nil && attempts > cap {
			return fmt.Errorf("RETRY_ATTEMPTS is %q, want <= %d: each attempt multiplies the load, and that many of them is the storm", vars["RETRY_ATTEMPTS"], cap)
		}
		return nil
	},
}

func ruleNames() []string {
	out := make([]string, 0, len(PolicyRules))
	for name := range PolicyRules {
		out = append(out, name)
	}
	return out
}

func isOff(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "off", "no", "":
		return true
	}
	return false
}
