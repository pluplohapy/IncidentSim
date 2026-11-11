// Package firecracker implements the runtime adapter on top of the Firecracker
// REST API.
//
// It is the production engine: a task runs in its own microVM with KVM, its
// rootfs is the Docker image of the workload service (a read-only layer plus a
// writable overlay), and a reset is a snapshot restore instead of a cold boot.
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// API is a client for the Firecracker REST API of one microVM.
//
// Firecracker listens on a unix socket per VM, so every call is a PUT to
// 127.0.0.1 through a custom dialer. The API surface used here is exactly the
// subset the simulator needs: boot source, machine config, drives, start,
// snapshot create and resume.
type API struct {
	// SocketPath is the API socket of the VM.
	SocketPath string
	// Timeout bounds every API call.
	Timeout time.Duration
	// VMMID is the guest instance id, used as the vsock guest id.
	VMMID string
}

// NewAPI returns a client for the VM addressed by socketPath.
func NewAPI(socketPath string) *API {
	return &API{SocketPath: socketPath, Timeout: 30 * time.Second}
}

func (a *API) client() *http.Client {
	return &http.Client{
		Timeout: a.Timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", a.SocketPath)
			},
		},
	}
}

// put sends a JSON body to a Firecracker endpoint.
func (a *API) put(ctx context.Context, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s: %w", path, err)
	}
	defer resp.Body.Close()
	body2, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("firecracker %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body2)))
	}
	return nil
}

// get reads a JSON document from a Firecracker endpoint.
func (a *API) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return err
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s: %w", path, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("firecracker %s: read body: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("firecracker %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}

// MachineConfig is the PUT /machine-config body.
type MachineConfig struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	SMT        bool `json:"smt"`
}

// BootSource is the PUT /boot-source body.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

// Drive is the PUT /drives/{id} body.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// InstanceAction is the PUT /actions body used to start a VM.
type InstanceAction struct {
	ActionType string `json:"action_type"`
}

// SnapshotCreate is the POST /snapshot/create body.
type SnapshotCreate struct {
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
	SnapshotType string `json:"snapshot_type"`
}

// SnapshotResume is the PATCH /snapshot/resume body.
type SnapshotResume struct {
	SnapshotPath   string `json:"snapshot_path"`
	MemFilePath    string `json:"mem_file_path"`
	EnableDiffSnap bool   `json:"enable_diff_snapshots"`
	ResumeVM       bool   `json:"resume_vm"`
}

// Configure applies the machine configuration.
func (a *API) Configure(ctx context.Context, cfg MachineConfig) error {
	return a.put(ctx, "/machine-config", cfg)
}

// SetBootSource applies the kernel and its command line.
func (a *API) SetBootSource(ctx context.Context, boot BootSource) error {
	return a.put(ctx, "/boot-source", boot)
}

// AddDrive attaches a block device. The first drive of a task is the read-only
// rootfs layer, the second the writable overlay.
func (a *API) AddDrive(ctx context.Context, drive Drive) error {
	return a.put(ctx, "/drives/"+drive.DriveID, drive)
}

// Start boots the guest.
func (a *API) Start(ctx context.Context) error {
	return a.put(ctx, "/actions", InstanceAction{ActionType: "InstanceStart"})
}

// InstanceInfo is the subset of GET /machine-config and instance state the
// adapter needs to confirm a VM came up.
type InstanceInfo struct {
	State string `json:"state"`
}

// Describe returns the state of the VM.
func (a *API) Describe(ctx context.Context) (InstanceInfo, error) {
	var info struct {
		State string `json:"state"`
	}
	if err := a.get(ctx, "/instance", &info); err != nil {
		return InstanceInfo{}, err
	}
	return InstanceInfo{State: info.State}, nil
}

// CreateSnapshot writes a full snapshot of the running VM.
func (a *API) CreateSnapshot(ctx context.Context, snapshot SnapshotCreate) error {
	return a.put(ctx, "/snapshot/create", snapshot)
}

// Resume restores a snapshot. Restoring rather than booting is what makes a
// task reset take milliseconds instead of seconds.
func (a *API) Resume(ctx context.Context, snapshot SnapshotResume) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, "http://localhost/snapshot/resume", nil)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return fmt.Errorf("firecracker resume: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("firecracker resume: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// Pause stops the VM without releasing it, so a snapshot can be taken of a
// consistent state.
func (a *API) Pause(ctx context.Context) error {
	return a.put(ctx, "/actions", InstanceAction{ActionType: "Pause"})
}
