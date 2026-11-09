package injector

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRegistryRegistersAndReturnsInjectors(t *testing.T) {
	registry := NewRegistry()
	stub := NewStub(KindFeatureFlag)
	registry.Register(stub)

	got, err := registry.Get(KindFeatureFlag)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != stub {
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
	registry.Register(NewStub(KindKill))
	replacement := NewStub(KindKill)
	registry.Register(replacement)
	got, err := registry.Get(KindKill)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != replacement {
		t.Fatal("second registration did not replace the first")
	}
}

func TestWithStubsCoversEveryKnownKind(t *testing.T) {
	registry := WithStubs()
	for _, kind := range Kinds() {
		if _, err := registry.Get(kind); err != nil {
			t.Fatalf("kind %s missing from stub registry: %v", kind, err)
		}
	}
}

func TestStubInjectorNeverChangesEnvironment(t *testing.T) {
	stub := NewStub(KindStress)
	err := stub.Activate(context.Background(), nil, Fault{Kind: KindStress, Target: "app"})
	if err == nil {
		t.Fatal("stub activated a fault without an environment")
	}
	if len(stub.Called) != 1 || stub.Called[0].Target != "app" {
		t.Fatalf("stub recorded %+v", stub.Called)
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
