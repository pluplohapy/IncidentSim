package contract

import (
	"testing"

	"incidentsim/internal/runtime"
)

// The in-memory adapter is always covered. Real Compose and Firecracker runs
// are opt-in because they need Docker or a provisioned Linux/KVM host.
func TestFakeAdapterContract(t *testing.T) {
	Run(t, func(t *testing.T) (runtime.Adapter, func()) {
		return runtime.NewFake("fake"), func() {}
	}, "fake")
}
