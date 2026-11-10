package verifier

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Policy rules of the quality gate.
//
// A rule is a named production expectation, referenced by name from a manifest
// check. Keeping the rules here, next to the checks, is what lets a task author
// express "this fix is production appropriate" without writing Go.
const (
	// RuleEnvFlagOff requires an environment flag to be unset or empty.
	RuleEnvFlagOff = "env_flag_off"
	// RuleEnvFlagEquals requires an environment variable to hold a value.
	RuleEnvFlagEquals = "env_flag_equals"
	// RuleServiceNotStopped requires the service process to be running: a
	// stopped service trivially "works" for no one.
	RuleServiceNotStopped = "service_not_stopped"
	// RuleLogOutputPreserved requires the log file of a service to exist and
	// to be non-empty: deleting the log is the classic distractor.
	RuleLogOutputPreserved = "log_output_preserved"
	// RuleLoggingNotDisabled requires the log stream of a service to exist.
	RuleLoggingNotDisabled = "logging_not_disabled"
	// RuleEnvIntAtLeast requires an environment variable to hold a number at
	// or above a threshold. It is how a retry policy is bounded without
	// forbidding retries, which is the difference between a bounded retry and
	// no retry at all.
	RuleEnvIntAtLeast = "env_int_at_least"
	// RuleRetryLoopBounded requires the retry policy of a service to keep
	// retrying and to back off. It is the chain-aware check: the amplifying
	// link of a fault chain is bounded rather than removed, because removing
	// retries turns a storm into a fast failure and only hides the loop.
	RuleRetryLoopBounded = "retry_loop_bounded"
)

// checkPolicy runs a policy check by looking its rule up in the registry.
//
// An unknown rule must fail the run rather than pass silently: a typo in a rule
// name would otherwise disable the quality gate for that check, which is the
// one failure mode that would make the whole simulator lie.
func (v *ManifestVerifier) checkPolicy(ctx context.Context, env Environment, check Check) Result {
	result := Result{Check: check}
	rule, ok := PolicyRules[check.Rule]
	if !ok {
		return fail(result, fmt.Sprintf("unknown policy rule %q (have %s)", check.Rule, ruleNames()))
	}
	vars, err := env.Env(ctx, check.Service)
	if err != nil {
		return fail(result, fmt.Sprintf("read environment of %q: %v", check.Service, err))
	}
	result.Evidence = fmt.Sprintf("%s=%q", check.Query, vars[check.Query])

	if err := rule(ctx, env, check, vars); err != nil {
		return fail(result, err.Error())
	}
	result.Passed = true
	result.Detail = fmt.Sprintf("policy %s satisfied", check.Rule)
	return result
}

// PolicyRules maps rule names to their evaluation.
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
	RuleServiceNotStopped: func(ctx context.Context, env Environment, check Check, vars map[string]string) error {
		// The service declares itself stopped only if it stopped answering at
		// all, which a health endpoint reports; here the rule reads the env of
		// the process that is supposed to serve it.
		if len(vars) == 0 {
			return fmt.Errorf("service %q has no environment: it is not running", check.Service)
		}
		return nil
	},
	RuleEnvIntAtLeast: func(ctx context.Context, env Environment, check Check, vars map[string]string) error {
		value, present := vars[check.Query]
		want := 0.0
		if check.Expect != nil && check.Expect.Min != nil {
			want = *check.Expect.Min
		}
		got, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || !present {
			return fmt.Errorf("%s is %q, expected a number >= %v", check.Query, value, want)
		}
		if got < want {
			return fmt.Errorf("%s is %v, want >= %v", check.Query, got, want)
		}
		return nil
	},
	RuleRetryLoopBounded: func(ctx context.Context, env Environment, check Check, vars map[string]string) error {
		// The retry policy must stay on, with a backoff that breaks the
		// amplification: that is the loop, and the loop is what has to be cut.
		enabled, present := vars["RETRY_ENABLED"]
		if !present || isOff(enabled) {
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
		return nil
	},
	RuleLogOutputPreserved: func(ctx context.Context, env Environment, check Check, vars map[string]string) error {
		path := vars[check.Query]
		if path == "" {
			return fmt.Errorf("%s is not set: the service does not know where it logs", check.Query)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("log file %s is gone: %v", path, err)
		}
		if info.Size() == 0 {
			return fmt.Errorf("log file %s is empty: the history was destroyed", path)
		}
		return nil
	},
}

// ruleNames lists the registered rules for an unknown-rule error message.
func ruleNames() []string {
	out := make([]string, 0, len(PolicyRules))
	for name := range PolicyRules {
		out = append(out, name)
	}
	return out
}

// isOff reports whether a flag value means "disabled".
func isOff(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "off", "no", "":
		return true
	}
	return false
}
