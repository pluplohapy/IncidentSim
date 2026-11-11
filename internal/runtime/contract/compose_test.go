package contract

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"incidentsim/internal/adapters/compose"
	"incidentsim/internal/runtime"
)

// dockerAvailable reports whether compose integration tests can run. They are
// skipped, not ignored: a developer without Docker still runs the contract
// suite against the fake. It mirrors internal/adapters/compose/adapter_test.go.
func dockerAvailable(t *testing.T) bool {
	t.Helper()
	if os.Getenv("INCIDENTSIM_TEST_DOCKER") != "1" {
		t.Skip("set INCIDENTSIM_TEST_DOCKER=1 to run compose contract tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not installed: %v", err)
	}
	client := compose.NewClient(t.TempDir(), "incidentsim-probe")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Available(ctx); err != nil {
		t.Skipf("docker compose not usable: %v", err)
	}
	return true
}

// TestComposeAdapterContract runs the contract suite against a real Docker
// Compose engine. It runs only when INCIDENTSIM_TEST_DOCKER=1 is set and a
// local docker daemon answers; otherwise it is skipped.
func TestComposeAdapterContract(t *testing.T) {
	if !dockerAvailable(t) {
		return
	}
	Run(t, func(t *testing.T) (runtime.Adapter, func()) {
		adapter := compose.New(t.TempDir())
		return adapter, func() {}
	}, "compose")
}
