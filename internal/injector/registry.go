package injector

import (
	"sort"
)

// DefaultRegistry returns the injectors the core registers in production.
//
// It deliberately contains only the injectors that are implemented. A manifest
// that declares a fault kind which is not here fails at session start: that is
// the difference between a task that does not start and a task that starts
// healthy and silently teaches the wrong lesson.
func DefaultRegistry() *Registry {
	registry := NewRegistry()
	registry.Register(NewFeatureFlagInjector())
	registry.Register(NewSQLConfigInjector())
	registry.Register(NewKillInjector())
	return registry
}

// Kinds returns the kinds the default registry covers, sorted.
func Kinds() []string {
	out := append([]string(nil), kindsList...)
	sort.Strings(out)
	return out
}

// kindsList is the closed set of fault kinds the manifest schema allows.
var kindsList = []string{KindFeatureFlag, KindSQLConfig, KindKill, KindNetwork, KindStress}
