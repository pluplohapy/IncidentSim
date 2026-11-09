package verifier

import (
	"context"
	"fmt"
	"strings"
)

// Policy rules. A rule is a named production expectation; a manifest check
// references it by name so the check itself stays declarative.
const (
	// RuleEnvFlagOff requires an environment flag to be unset or empty.
	RuleEnvFlagOff = "env_flag_off"
	// RuleEnvFlagEquals requires an environment variable to hold a value.
	RuleEnvFlagEquals = "env_flag_equals"
)

// checkPolicy runs a policy check by looking its rule up in the registry.
//
// Policy checks are the difference between "the system answers" and "the fix
// belongs in production", so an unknown rule must fail the run rather than
// pass silently: a typo in a rule name would otherwise disable the quality
// gate for that check.
func (v *ManifestVerifier) checkPolicy(ctx context.Context, env Environment, check Check) Result {
	result := Result{Check: check}
	rule, ok := PolicyRules[check.Rule]
	if !ok {
		return fail(result, fmt.Sprintf("unknown policy rule %q", check.Rule))
	}
	vars, err := env.Env(ctx, check.Service)
	if err != nil {
		return fail(result, fmt.Sprintf("read environment of %q: %v", check.Service, err))
	}
	result.Evidence = fmt.Sprintf("%s=%q", check.Query, vars[check.Query])

	if err := rule(check, vars); err != nil {
		return fail(result, err.Error())
	}
	result.Passed = true
	result.Detail = fmt.Sprintf("policy %s satisfied", check.Rule)
	return result
}

// PolicyRules maps rule names to their evaluation.
var PolicyRules = map[string]func(check Check, vars map[string]string) error{
	RuleEnvFlagOff: func(check Check, vars map[string]string) error {
		value, present := vars[check.Query]
		if !present || strings.TrimSpace(value) == "" || isOff(value) {
			return nil
		}
		return fmt.Errorf("%s is still %q, expected the flag to be off", check.Query, value)
	},
	RuleEnvFlagEquals: func(check Check, vars map[string]string) error {
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
}

// isOff reports whether a flag value means "disabled".
func isOff(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "off", "no", "":
		return true
	}
	return false
}
