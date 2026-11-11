package contract

import (
	"testing"

	"incidentsim/internal/runtime"
)

// TestFakeAdapterContract runs the contract suite against the in-memory fake
// adapter. It requires neither Docker nor KVM and must pass on every machine.
func TestFakeAdapterContract(t *testing.T) {
	Run(t, func(t *testing.T) (runtime.Adapter, func()) {
		return runtime.NewFake("compose"), func() {}
	}, "fake-compose")
}
