package injector

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type testInjector struct{ kind string }

func (i testInjector) Kind() string { return i.kind }

func (testInjector) Activate(_ context.Context, _ Environment, _ Fault) error { return nil }

func TestRegistryRegistersAndReturnsInjectors(t *testing.T) {
	registry := NewRegistry()
	registered := testInjector{kind: KindFeatureFlag}
	registry.Register(registered)

	got, err := registry.Get(KindFeatureFlag)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != registered {
		t.Fatal("registry returned a different injector")
	}
	if kinds := registry.Kinds(); len(kinds) != 1 || kinds[0] != KindFeatureFlag {
		t.Fatalf("kinds = %v", kinds)
	}
}

func TestRegistryReportsUnknownKind(t *testing.T) {
	registry := NewRegistry()
	_, err := registry.Get(KindNetwork)
	if !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("error = %v, want ErrUnknownKind", err)
	}
	if !strings.Contains(err.Error(), "none") {
		t.Fatalf("error does not mention the empty registry: %v", err)
	}
}

func TestRegistryReplacesDuplicateKind(t *testing.T) {
	registry := NewRegistry()
	registry.Register(testInjector{kind: KindKill})
	replacement := testInjector{kind: KindKill}
	registry.Register(replacement)
	got, err := registry.Get(KindKill)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != replacement {
		t.Fatal("second registration did not replace the first")
	}
}

func TestFaultKindConstantsMatchSchema(t *testing.T) {
	want := []string{KindFeatureFlag, KindKill, KindNetwork, KindSQLConfig, KindStress}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
}
