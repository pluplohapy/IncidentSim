package injector

import (
	"context"
	"fmt"
)

// FeatureFlagInjector injects a config-level fault by setting environment
// variables and restarting the target service.
//
// This is the injector behind config-level tasks: log verbosity, a feature
// switch, a pool size. The environment is the whole mechanism, which is why a
// fix is expressible as a config change and why a policy check can see it.
type FeatureFlagInjector struct{}

// NewFeatureFlagInjector returns the config-level fault injector.
func NewFeatureFlagInjector() *FeatureFlagInjector { return &FeatureFlagInjector{} }

// Kind implements Injector.
func (i *FeatureFlagInjector) Kind() string { return KindFeatureFlag }

// Activate implements Injector.
func (i *FeatureFlagInjector) Activate(ctx context.Context, env Environment, fault Fault) error {
	vars := NewOptions(fault.Options).Env()
	if len(vars) == 0 {
		return fmt.Errorf("%w: feature_flag fault on %q declares no env", ErrBadOptions, fault.Target)
	}
	if err := env.SetEnv(ctx, fault.Target, vars); err != nil {
		return fmt.Errorf("set fault env on %s: %w", fault.Target, err)
	}
	return nil
}
