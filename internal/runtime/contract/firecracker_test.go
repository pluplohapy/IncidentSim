package contract

import (
	"os"
	"testing"

	"incidentsim/internal/adapters/firecracker"
	"incidentsim/internal/runtime"
)

// TestFirecrackerAdapterContract is intentionally opt-in: it needs a Linux KVM
// host provisioned with the rootfs, bridge and guest-agent artifacts. Keeping
// the skip explicit prevents a laptop from claiming the microVM engine passed.
func TestFirecrackerAdapterContract(t *testing.T) {
	if os.Getenv("INCIDENTSIM_TEST_FIRECRACKER") != "1" {
		t.Skip("set INCIDENTSIM_TEST_FIRECRACKER=1 on a provisioned KVM host")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	Run(t, func(t *testing.T) (runtime.Adapter, func()) {
		adapter := firecracker.New(firecracker.DefaultConfig())
		return adapter, func() {}
	}, "firecracker")
}
