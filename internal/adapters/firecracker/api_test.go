package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"incidentsim/internal/runtime"
)

// shortSocketPath returns a unix socket path usable on macOS, where the
// pathname limit is short enough that t.TempDir() overflows it.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	// The socket path must stay short: macOS caps a unix pathname well below
	// the length of t.TempDir().
	path := "/tmp/im-" + strconv.Itoa(int(time.Now().UnixNano())) + ".sock"
	t.Cleanup(func() { os.Remove(path) })
	return path
}

// apiFake records the requests a Firecracker VM would receive.
type apiFake struct {
	mu       sync.Mutex
	requests []string
	bodies   map[string]map[string]any
}

func newAPIFake(t *testing.T) (*apiFake, *API) {
	t.Helper()
	socket := shortSocketPath(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &apiFake{bodies: map[string]map[string]any{}}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(fake.handle)}}
	server.Start()
	t.Cleanup(server.Close)
	return fake, NewAPI(socket)
}

func (f *apiFake) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.bodies[r.Method+" "+r.URL.Path] = body
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		_, _ = w.Write([]byte(`{"state":"Running"}`))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *apiFake) seen(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests {
		if request == path {
			return true
		}
	}
	return false
}

func (f *apiFake) body(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[path]
}

func TestAPISendsMachineConfiguration(t *testing.T) {
	fake, api := newAPIFake(t)
	if err := api.Configure(context.Background(), MachineConfig{VCPUCount: 2, MemSizeMiB: 512}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !fake.seen("PUT /machine-config") {
		t.Fatalf("requests = %v", fake.requests)
	}
	if body := fake.body("PUT /machine-config"); body["vcpu_count"] != float64(2) || body["mem_size_mib"] != float64(512) {
		t.Fatalf("machine config body = %v", body)
	}
}

func TestAPIAttachesRootfsReadOnlyAndOverlayWritable(t *testing.T) {
	fake, api := newAPIFake(t)
	ctx := context.Background()

	if err := api.AddDrive(ctx, Drive{DriveID: "rootfs", PathOnHost: "/rootfs.squashfs", IsRootDevice: true, IsReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := api.AddDrive(ctx, Drive{DriveID: "overlay", PathOnHost: "/overlay.ext4", IsRootDevice: false, IsReadOnly: false}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"rootfs", "overlay"} {
		if !fake.seen("PUT /drives/" + id) {
			t.Fatalf("drive %s was not attached: %v", id, fake.requests)
		}
	}
	if body := fake.body("PUT /drives/rootfs"); body["is_read_only"] != true {
		t.Fatalf("rootfs body = %v: the base layer must be read-only", body)
	}
	if body := fake.body("PUT /drives/overlay"); body["is_read_only"] != false {
		t.Fatalf("overlay body = %v: the overlay must be writable", body)
	}
}

func TestAPIStartsTheVM(t *testing.T) {
	fake, api := newAPIFake(t)
	if err := api.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if body := fake.body("PUT /actions"); body["action_type"] != "InstanceStart" {
		t.Fatalf("start body = %v", body)
	}
}

func TestAPIDescribeUsesInstanceRootEndpoint(t *testing.T) {
	fake, api := newAPIFake(t)
	info, err := api.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.State != "Running" {
		t.Fatalf("state = %q, want Running", info.State)
	}
	if !fake.seen("GET /") || fake.seen("GET /instance") {
		t.Fatalf("describe requests = %v", fake.requests)
	}
}

func TestAPISnapshotCreateAndLoad(t *testing.T) {
	fake, api := newAPIFake(t)
	ctx := context.Background()

	if err := api.CreateSnapshot(ctx, SnapshotCreate{SnapshotPath: "/s/base.snap", MemFilePath: "/s/base.mem", SnapshotType: "Full"}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if body := fake.body("PUT /snapshot/create"); body["snapshot_path"] != "/s/base.snap" {
		t.Fatalf("snapshot body = %v", body)
	}

	if err := api.LoadSnapshot(ctx, SnapshotResume{SnapshotPath: "/s/base.snap", MemFilePath: "/s/base.mem", ResumeVM: true}); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if !fake.seen("PUT /snapshot/load") {
		t.Fatalf("snapshot load was not sent correctly: %v", fake.requests)
	}
	if body := fake.body("PUT /snapshot/load"); body["resume_vm"] != true {
		t.Fatalf("load body = %v", body)
	}
}

func TestAPIBootArgsCarryTheAgentPort(t *testing.T) {
	adapter := New(Config{KernelImage: "/vmlinux", GuestAgentPort: 51000})
	machine := &vm{id: "task-1"}
	args := adapter.bootArgs(machine)
	for _, want := range []string{"root=/dev/vda", "console=ttyS0", "ip=", "eth0:off", "incidentsim.agent_port=51000"} {
		if !strings.Contains(args, want) {
			t.Fatalf("boot args %q missing %q", args, want)
		}
	}
}

func TestAdapterRefusesToRunWithoutKVM(t *testing.T) {
	// This is the honest answer on a Mac: no /dev/kvm, no microVM.
	if _, err := os.Stat("/dev/kvm"); err == nil {
		t.Skip("this host has KVM")
	}
	adapter := New(DefaultConfig())
	_, err := adapter.Up(context.Background(), "session-1", Spec())
	if !errors.Is(err, runtime.ErrUnsupported) {
		t.Fatalf("Up error = %v, want the unsupported-runtime error", err)
	}
}

func TestBootArgsAndConfigDefaults(t *testing.T) {
	if DefaultConfig().GuestAgentPort == 0 {
		t.Fatal("default guest agent port is unset")
	}
	if cpuCount(Spec()) != 1 {
		t.Fatalf("default cpu count = %d", cpuCount(Spec()))
	}
	if memoryMiB(Spec()) != 256 {
		t.Fatalf("default memory = %d MiB", memoryMiB(Spec()))
	}
}

func TestSessionNetworkAllocationIsStableAndDistinct(t *testing.T) {
	first := guestIPForSession("session-a")
	if first != guestIPForSession("session-a") {
		t.Fatal("guest IP allocation is not stable")
	}
	if first == guestIPForSession("session-b") {
		t.Fatal("different sessions received the same guest IP")
	}
	if tapForSession("session-a", "incidentsim-tap0") == tapForSession("session-b", "incidentsim-tap0") {
		t.Fatal("different sessions received the same tap")
	}
}
