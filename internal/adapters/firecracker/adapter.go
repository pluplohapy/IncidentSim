package firecracker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"incidentsim/internal/runtime"
)

// Config points the adapter at the artefacts Ansible provisions.
type Config struct {
	// Version is the Firecracker API/binary version provisioned by Ansible.
	Version string
	// FirecrackerBin is the firecracker binary.
	FirecrackerBin string
	// SocketDir holds the API sockets of running VMs.
	SocketDir string
	// KernelImage is the guest kernel.
	KernelImage string
	// RootfsDir holds the per-task rootfs images.
	RootfsDir string
	// BaselineDir holds the baseline snapshots.
	BaselineDir string
	// GuestAgentPort is the vsock port the guest agent listens on.
	GuestAgentPort uint32
	// GuestIP and TapDevice expose guest service ports to core health/replay
	// checks. They are host-only network addresses, never public bindings.
	GuestIP   string
	TapDevice string
	// FirecrackerBinArgs are extra firecracker flags, e.g. --seccomp-filter.
	ExtraArgs []string
}

// DefaultConfig returns the layout the Ansible role installs.
func DefaultConfig() Config {
	return Config{
		FirecrackerBin: "/usr/local/bin/firecracker",
		Version:        "1.11.0",
		SocketDir:      "/var/lib/incidentsim/vms",
		KernelImage:    "/var/lib/incidentsim/vmlinux",
		RootfsDir:      "/var/lib/incidentsim/rootfs",
		BaselineDir:    "/var/lib/incidentsim/snapshots",
		GuestAgentPort: 51000,
		GuestIP:        "172.30.0.2",
		TapDevice:      "incidentsim-tap0",
	}
}

// Adapter implements runtime.Adapter with a Firecracker microVM per session.
//
// The rootfs of the VM is the Docker image of the workload service, exported as
// a squashfs layer plus a writable overlay. That is what lets a task written
// for compose run unchanged in a microVM: the service is the same binary, only
// the isolation around it changes.
type Adapter struct {
	Config Config
	// Bin is the firecracker binary, overridable in tests.
	Bin string

	mu       sync.Mutex
	sessions map[string]*vm
}

// vm is one running microVM.
type vm struct {
	id       string
	api      *API
	workdir  string
	spec     runtime.Spec
	baseline runtime.Spec
	rootfs   string
	overlay  string
	snapshot string
	started  time.Time
	ports    map[string]int
	pid      int
	process  *exec.Cmd
}

// New returns an adapter using the given configuration.
func New(config Config) *Adapter {
	return &Adapter{Config: config, Bin: config.FirecrackerBin, sessions: map[string]*vm{}}
}

// Name implements runtime.Adapter.
func (a *Adapter) Name() string { return "firecracker" }

// Up implements runtime.Adapter.
func (a *Adapter) Up(ctx context.Context, sessionID string, spec runtime.Spec) (*runtime.Instance, error) {
	if err := a.available(ctx); err != nil {
		return nil, err
	}

	a.mu.Lock()
	if existing, ok := a.sessions[sessionID]; ok {
		a.mu.Unlock()
		return a.describe(existing), nil
	}
	machine := &vm{
		id:       sessionID,
		api:      NewAPI(filepath.Join(a.Config.SocketDir, sessionID+".sock")),
		workdir:  filepath.Join(a.Config.SocketDir, sessionID),
		spec:     cloneSpec(spec),
		baseline: cloneSpec(spec),
		ports:    map[string]int{},
	}
	a.sessions[sessionID] = machine
	a.mu.Unlock()
	started := false
	defer func() {
		if started {
			return
		}
		if machine.process != nil && machine.process.Process != nil {
			_ = machine.process.Process.Kill()
			_, _ = machine.process.Process.Wait()
		}
		_ = os.RemoveAll(machine.workdir)
		a.mu.Lock()
		delete(a.sessions, sessionID)
		a.mu.Unlock()
	}()

	if err := os.MkdirAll(machine.workdir, 0o755); err != nil {
		return nil, fmt.Errorf("create vm dir: %w", err)
	}
	rootfs, err := a.ensureRootfs(ctx, machine)
	if err != nil {
		return nil, err
	}
	machine.rootfs = rootfs
	machine.overlay = filepath.Join(machine.workdir, "overlay.ext4")
	if err := createOverlay(machine.overlay); err != nil {
		return nil, err
	}
	if err := a.launch(ctx, machine); err != nil {
		return nil, err
	}

	if err := a.writeGuestConfig(machine); err != nil {
		return nil, err
	}
	if err := a.start(ctx, machine); err != nil {
		return nil, err
	}
	if err := a.waitForAgent(ctx, machine); err != nil {
		return nil, err
	}
	machine.started = time.Now()
	instance := a.describe(machine)
	if err := runtime.WaitForHealth(ctx, instance, runtime.DefaultUpTimeout); err != nil {
		return nil, err
	}
	started = true
	return instance, nil
}

// available reports whether the host can run Firecracker at all. On macOS this
// is where the engine truthfully says no: there is no /dev/kvm, and emulating
// it is not supported.
func (a *Adapter) available(ctx context.Context) error {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("%w: /dev/kvm is missing, so Firecracker cannot run on this host (use the compose engine)", runtime.ErrUnsupported)
	}
	if _, err := exec.LookPath(a.Bin); err != nil {
		return fmt.Errorf("%w: %v", runtime.ErrUnsupported, err)
	}
	return nil
}

// ensureRootfs materialises the read-only rootfs layer and the writable overlay
// of a task. The layer is exported from the workload image, which is what makes
// the same service work in compose and in a microVM.
func (a *Adapter) ensureRootfs(ctx context.Context, machine *vm) (string, error) {
	images := map[string]string{}
	for _, service := range machine.spec.Services {
		images[service.Name] = service.Image
	}
	if len(images) != 1 {
		// One rootfs per VM keeps the adapter simple: a task is one service
		// plus, optionally, a database service the guest agent starts too.
		return "", fmt.Errorf("%w: the firecracker engine supports exactly one rootfs image per VM", runtime.ErrUnsupported)
	}
	var image string
	for _, value := range images {
		image = value
	}
	rootfs := filepath.Join(a.Config.RootfsDir, sanitise(image)+".squashfs")
	if _, err := os.Stat(rootfs); err == nil {
		return rootfs, nil
	}
	if err := a.exportImage(ctx, image, rootfs); err != nil {
		return "", err
	}
	return rootfs, nil
}

// exportImage runs `docker create` + `docker export` + `mksquashfs` to turn a
// container image into a rootfs layer. It is the bridge from the compose world
// to the microVM world.
func (a *Adapter) exportImage(ctx context.Context, image, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("%w: docker is needed to export the rootfs", runtime.ErrUnsupported)
	}
	if _, err := exec.LookPath("mksquashfs"); err != nil {
		return fmt.Errorf("%w: mksquashfs is needed to build the rootfs layer", runtime.ErrUnsupported)
	}
	container, err := runCommand(ctx, "", "docker", "create", image)
	if err != nil {
		return fmt.Errorf("create container from %s: %w", image, err)
	}
	defer func() { _, _ = runCommand(context.Background(), "", "docker", "rm", "-f", container) }()

	raw := filepath.Join(filepath.Dir(target), sanitise(image)+".tar")
	if _, err := runCommand(ctx, "", "docker", "export", container, "-o", raw); err != nil {
		return fmt.Errorf("export %s: %w", image, err)
	}
	defer os.Remove(raw)
	if _, err := runCommand(ctx, "", "mksquashfs", raw, target, "-noappend"); err != nil {
		return fmt.Errorf("mksquashfs %s: %w", image, err)
	}
	return nil
}

// start boots the VM and attaches its drives.
func (a *Adapter) start(ctx context.Context, machine *vm) error {
	if err := machine.api.Configure(ctx, MachineConfig{
		VCPUCount:  cpuCount(machine.spec),
		MemSizeMiB: memoryMiB(machine.spec),
		SMT:        false,
	}); err != nil {
		return fmt.Errorf("configure machine: %w", err)
	}
	if err := machine.api.SetBootSource(ctx, BootSource{
		KernelImagePath: a.Config.KernelImage,
		BootArgs:        a.bootArgs(machine),
	}); err != nil {
		return fmt.Errorf("set boot source: %w", err)
	}
	if err := machine.api.AddDrive(ctx, Drive{
		DriveID:      "rootfs",
		PathOnHost:   machine.rootfs,
		IsRootDevice: true,
		IsReadOnly:   true,
	}); err != nil {
		return fmt.Errorf("attach rootfs: %w", err)
	}
	if err := machine.api.AddDrive(ctx, Drive{
		DriveID:      "overlay",
		PathOnHost:   machine.overlay,
		IsRootDevice: false,
		IsReadOnly:   false,
	}); err != nil {
		return fmt.Errorf("attach overlay: %w", err)
	}
	if err := machine.api.AddVsock(ctx, VsockDevice{
		GuestCID: vsockGuestCID,
		UDSPath:  filepath.Join(machine.workdir, "vsock.sock"),
	}); err != nil {
		return fmt.Errorf("configure vsock: %w", err)
	}
	if a.Config.TapDevice != "" {
		if err := machine.api.AddNetworkInterface(ctx, NetworkInterface{
			IfaceID: "eth0", HostDevName: a.Config.TapDevice, GuestMAC: "AA:FC:00:00:00:02",
		}); err != nil {
			return fmt.Errorf("configure network: %w", err)
		}
	}
	return machine.api.Start(ctx)
}

// bootArgs builds the guest kernel command line. The guest agent reads its
// configuration from the overlay, and the task id travels on the command line.
func (a *Adapter) bootArgs(machine *vm) string {
	args := []string{
		"console=ttyS0",
		"panic=1",
		"pci=off",
		"ip=off",
		"incidentsim.task=" + machine.id,
		"incidentsim.agent_port=" + fmt.Sprint(a.Config.GuestAgentPort),
		"root=/dev/vda",
		"rw",
	}
	return strings.Join(args, " ")
}

// waitForAgent polls the guest agent over vsock until it answers.
func (a *Adapter) waitForAgent(ctx context.Context, machine *vm) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		conn, err := a.dialAgent(ctx, machine)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("guest agent did not come up: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// dialAgent connects to the guest agent over vsock.
func (a *Adapter) dialAgent(ctx context.Context, machine *vm) (net.Conn, error) {
	return vsockDial(ctx, filepath.Join(machine.workdir, "vsock.sock"), a.Config.GuestAgentPort)
}

// writeGuestConfig writes the configuration the guest agent reads at boot.
func (a *Adapter) writeGuestConfig(machine *vm) error {
	config := guestConfigFile{}
	for _, service := range machine.spec.Services {
		port := 0
		if len(service.Ports) > 0 {
			port = service.Ports[0]
		}
		config.Services = append(config.Services, guestService{
			Name:    service.Name,
			Image:   service.Image,
			Command: service.Command,
			Env:     service.Env,
			Port:    port,
		})
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(machine.workdir, "guest-config.json"), data, 0o600)
}

// describe projects the VM into the runtime contract.
func (a *Adapter) describe(machine *vm) *runtime.Instance {
	services := make([]runtime.Service, 0, len(machine.spec.Services))
	for _, service := range machine.spec.Services {
		port := 0
		if len(service.Ports) > 0 {
			port = service.Ports[0]
		}
		endpoint := fmt.Sprintf("%s:%d", a.Config.GuestIP, port)
		services = append(services, runtime.Service{
			Name:          service.Name,
			Endpoint:      endpoint,
			ContainerPort: port,
			Healthcheck:   service.Healthcheck,
		})
	}
	return &runtime.Instance{
		ID:        machine.id,
		Engine:    "firecracker",
		Services:  services,
		Workdir:   machine.workdir,
		StartedAt: machine.started,
	}
}

// Reset implements runtime.Adapter: it restores the baseline snapshot instead
// of rebooting, which is what makes a reset fast enough to be part of a
// session.
func (a *Adapter) Reset(ctx context.Context, sessionID string) (*runtime.Instance, error) {
	machine, err := a.machine(sessionID)
	if err != nil {
		return nil, err
	}
	if err := a.pauseOrKill(ctx, machine); err != nil {
		return nil, err
	}
	snapshot := filepath.Join(a.Config.BaselineDir, machine.snapshotName())
	if _, err := os.Stat(snapshot); err != nil {
		return nil, fmt.Errorf("baseline snapshot %s is missing: run `make ansible-provision`", snapshot)
	}
	if err := machine.api.LoadSnapshot(ctx, SnapshotResume{
		SnapshotPath:   snapshot,
		MemFilePath:    snapshot + ".mem",
		EnableDiffSnap: true,
		ResumeVM:       true,
	}); err != nil {
		return nil, fmt.Errorf("restore snapshot: %w", err)
	}
	machine.spec = cloneSpec(machine.baseline)
	if err := a.writeGuestConfig(machine); err != nil {
		return nil, err
	}
	if err := a.waitForAgent(ctx, machine); err != nil {
		return nil, err
	}
	machine.started = time.Now()
	return a.describe(machine), nil
}

// pauseOrKill stops a VM so a snapshot restore can reopen its files.
func (a *Adapter) pauseOrKill(ctx context.Context, machine *vm) error {
	_ = machine.api.Pause(ctx)
	// Firecracker exits when its process is terminated; the adapter owns that
	// process through the systemd unit.
	return nil
}

// Apply implements runtime.Adapter: a config change is written to the overlay
// and the guest agent restarts the service with the new environment.
func (a *Adapter) Apply(ctx context.Context, sessionID string, patch runtime.EnvPatch) (*runtime.Instance, error) {
	if patch.Image != "" {
		return nil, fmt.Errorf("%w: image patches are not supported; deploy a binary artifact", runtime.ErrUnsupported)
	}
	machine, err := a.machine(sessionID)
	if err != nil {
		return nil, err
	}
	conn, err := a.dialAgent(ctx, machine)
	if err != nil {
		return nil, fmt.Errorf("reach guest agent: %w", err)
	}
	defer conn.Close()

	request := map[string]any{"service": patch.Service, "env": patch.Vars}
	if err := postJSON(conn, "/restart", request); err != nil {
		return nil, err
	}
	var response struct {
		Restarted string `json:"restarted"`
	}
	if err := readJSON(conn, &response); err != nil {
		return nil, err
	}
	if response.Restarted != patch.Service {
		return nil, fmt.Errorf("guest agent restarted %q, expected %q", response.Restarted, patch.Service)
	}

	for _, service := range machine.spec.Services {
		if service.Name != patch.Service {
			continue
		}
		if service.Env == nil {
			service.Env = map[string]string{}
		}
		for key, value := range patch.Vars {
			if value == "" {
				delete(service.Env, key)
				continue
			}
			service.Env[key] = value
		}
	}
	return a.describe(machine), nil
}

// Deploy installs an artifact into the Firecracker guest. The implementation
// is finalised in H1; until then it reports unsupported so the interface is
// satisfied everywhere.
func (a *Adapter) Deploy(ctx context.Context, sessionID string, art runtime.Artifact, restart bool) (*runtime.Instance, error) {
	if _, err := a.machine(sessionID); err != nil {
		return nil, err
	}
	machine, err := a.machine(sessionID)
	if err != nil {
		return nil, err
	}
	if art.Service == "" {
		return nil, fmt.Errorf("artifact service is required")
	}
	if art.Kind != "binary" && art.Kind != "files" {
		return nil, fmt.Errorf("%w: artifact kind %q", runtime.ErrUnsupported, art.Kind)
	}
	conn, err := a.dialAgent(ctx, machine)
	if err != nil {
		return nil, fmt.Errorf("reach guest agent: %w", err)
	}
	defer conn.Close()
	request := map[string]any{"service": art.Service, "kind": art.Kind, "restart": restart, "files": art.Files}
	if art.Kind == "binary" {
		data, readErr := os.ReadFile(art.Path)
		if readErr != nil {
			return nil, fmt.Errorf("read artifact: %w", readErr)
		}
		request["binary"] = data
	}
	if err := postJSON(conn, "/put-file", request); err != nil {
		return nil, err
	}
	var response struct {
		Deployed string `json:"deployed"`
	}
	if err := readJSON(conn, &response); err != nil {
		return nil, err
	}
	if response.Deployed != art.Service {
		return nil, fmt.Errorf("guest deployed %q, expected %q", response.Deployed, art.Service)
	}
	return a.describe(machine), nil
}

// Exec implements runtime.Adapter through the guest agent.
func (a *Adapter) Exec(ctx context.Context, sessionID string, req runtime.ExecRequest) (runtime.ExecResult, error) {
	machine, err := a.machine(sessionID)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	conn, err := a.dialAgent(ctx, machine)
	if err != nil {
		return runtime.ExecResult{}, fmt.Errorf("reach guest agent: %w", err)
	}
	defer conn.Close()

	start := time.Now()
	if err := postJSON(conn, "/exec", map[string]any{
		"service":    req.Service,
		"command":    req.Command,
		"env":        req.Env,
		"timeout_ms": int(req.Timeout.Milliseconds()),
	}); err != nil {
		return runtime.ExecResult{}, err
	}
	var response struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := readJSON(conn, &response); err != nil {
		return runtime.ExecResult{}, err
	}
	return runtime.ExecResult{
		ExitCode: response.ExitCode,
		Stdout:   response.Stdout,
		Stderr:   response.Stderr,
		Duration: time.Since(start),
	}, nil
}

// Logs implements runtime.Adapter through the guest agent.
func (a *Adapter) Logs(ctx context.Context, sessionID string, opts runtime.LogOptions) ([]runtime.LogLine, error) {
	machine, err := a.machine(sessionID)
	if err != nil {
		return nil, err
	}
	conn, err := a.dialAgent(ctx, machine)
	if err != nil {
		return nil, fmt.Errorf("reach guest agent: %w", err)
	}
	defer conn.Close()

	if err := postJSON(conn, "/logs", map[string]any{"service": opts.Service, "tail": opts.Tail}); err != nil {
		return nil, err
	}
	var response struct {
		Lines []string `json:"lines"`
	}
	if err := readJSON(conn, &response); err != nil {
		return nil, err
	}
	out := make([]runtime.LogLine, 0, len(response.Lines))
	for _, line := range response.Lines {
		if opts.Grep != "" && !strings.Contains(line, opts.Grep) {
			continue
		}
		out = append(out, runtime.LogLine{Service: opts.Service, Text: line})
	}
	return out, nil
}

// Metrics implements runtime.Adapter through the guest agent.
func (a *Adapter) Metrics(ctx context.Context, sessionID string, service string) ([]runtime.MetricSample, error) {
	machine, err := a.machine(sessionID)
	if err != nil {
		return nil, err
	}
	conn, err := a.dialAgent(ctx, machine)
	if err != nil {
		return nil, fmt.Errorf("reach guest agent: %w", err)
	}
	defer conn.Close()

	if err := postJSON(conn, "/metrics", map[string]any{"service": service}); err != nil {
		return nil, err
	}
	var response struct {
		Env map[string]string `json:"env"`
	}
	if err := readJSON(conn, &response); err != nil {
		return nil, err
	}
	// The guest agent reports the environment of a service; the adapter does
	// not invent metrics of its own.
	sample := runtime.MetricSample{
		Service: service,
		Name:    "guest_service_env",
		Labels:  response.Env,
		At:      time.Now(),
	}
	return []runtime.MetricSample{sample}, nil
}

// Down implements runtime.Adapter.
func (a *Adapter) Down(ctx context.Context, sessionID string) error {
	machine, err := a.machine(sessionID)
	if err != nil {
		if errors.Is(err, runtime.ErrNotFound) {
			return nil
		}
		return err
	}
	_ = machine.api.Pause(ctx)
	if machine.process != nil && machine.process.Process != nil {
		_ = machine.process.Process.Kill()
		_, _ = machine.process.Process.Wait()
	}
	a.mu.Lock()
	delete(a.sessions, sessionID)
	a.mu.Unlock()
	return nil
}

func (a *Adapter) launch(ctx context.Context, machine *vm) error {
	if err := os.RemoveAll(machine.api.SocketPath); err != nil {
		return err
	}
	args := append([]string{"--api-sock", machine.api.SocketPath}, a.Config.ExtraArgs...)
	cmd := exec.CommandContext(ctx, a.Bin, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start firecracker: %w", err)
	}
	machine.process, machine.pid = cmd, cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(machine.api.SocketPath); err == nil {
			return nil
		}
		if cmd.ProcessState != nil {
			return fmt.Errorf("firecracker exited before opening API socket")
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return fmt.Errorf("firecracker did not create API socket %s", machine.api.SocketPath)
}

func createOverlay(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create overlay: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(512 << 20); err != nil {
		return fmt.Errorf("size overlay: %w", err)
	}
	f.Close()
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		return fmt.Errorf("%w: mkfs.ext4 is required to format the Firecracker overlay", runtime.ErrUnsupported)
	}
	cmd := exec.Command("mkfs.ext4", "-F", "-q", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("format overlay: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// vsockDial speaks Firecracker's UDS CONNECT protocol. The response starts
// with OK on success and leaves the stream connected to the guest port.
func vsockDial(ctx context.Context, socket string, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(line, "OK") {
		conn.Close()
		return nil, fmt.Errorf("vsock connect rejected: %s", strings.TrimSpace(line))
	}
	return conn, nil
}

func (a *Adapter) machine(sessionID string) (*vm, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	machine, ok := a.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", runtime.ErrNotFound, sessionID)
	}
	return machine, nil
}

func (machine *vm) snapshotName() string {
	return machine.id + ".snap"
}

// vsockGuestCID is the guest id the adapter addresses. Firecracker assigns
// guest ids per VM; the adapter uses a fixed one so the guest agent can bind a
// deterministic port.
const vsockGuestCID = 3

func cpuCount(spec runtime.Spec) int {
	if spec.CPUCount > 0 {
		return spec.CPUCount
	}
	return 1
}

func memoryMiB(spec runtime.Spec) int {
	if spec.MemoryMiB > 0 {
		return spec.MemoryMiB
	}
	return 256
}

// cloneSpec deep copies a spec so patching the live state never mutates the
// baseline a reset restores.
func cloneSpec(spec runtime.Spec) runtime.Spec {
	out := spec
	out.Services = make([]runtime.ServiceSpec, 0, len(spec.Services))
	for _, service := range spec.Services {
		clone := service
		if service.Env != nil {
			clone.Env = make(map[string]string, len(service.Env))
			for key, value := range service.Env {
				clone.Env[key] = value
			}
		}
		out.Services = append(out.Services, clone)
	}
	return out
}

func sanitise(image string) string {
	replacer := strings.NewReplacer("/", "_", ":", "-", "@", "_")
	return replacer.Replace(image)
}

func runCommand(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// Spec returns the manifest projection used by the adapter tests.
func Spec() runtime.Spec {
	return runtime.Spec{
		Services: []runtime.ServiceSpec{{
			Name:  "app",
			Image: "incidentsim/workload:dev",
			Ports: []int{8080},
		}},
	}
}
