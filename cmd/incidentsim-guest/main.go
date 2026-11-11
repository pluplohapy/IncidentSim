// Command incidentsim-guest is the agent that runs inside a task microVM.
//
// Firecracker has no shell channel into the guest, so everything the core needs
// from inside the VM goes through this agent over vsock: exec, logs, metrics and
// the environment a service is running with. It is deliberately tiny: the guest
// is an untrusted environment by construction, and the agent exposes exactly
// the operations the adapter is allowed to perform.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultCID is the vsock guest id the adapter connects to.
	defaultCID = 3
	// configPath is where the adapter and the boot args place the service
	// configuration. The agent re-reads it on every restart, which is how a
	// config change becomes a real change.
	configPath = "/run/incidentsim/config.json"
)

// serviceConfig is the environment of one service inside the guest.
type serviceConfig struct {
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Command []string          `json:"command,omitempty"`
	// Entrypoint replaces the default rootfs binary; it wins over Command
	// when both are set, matching compose's entrypoint/command split.
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Port      int               `json:"port,omitempty"`
}

// guestConfig is the whole configuration of a task microVM.
type guestConfig struct {
	Services []serviceConfig `json:"services"`
}

// agent serves the core's requests over vsock.
type agent struct {
	mu      sync.Mutex
	config  guestConfig
	procs   map[string]*serviceProcess
	started time.Time
	// configPath is where the live configuration is persisted, so a change
	// survives a snapshot restore.
	configPath string
}

// serviceProcess tracks one running service of the guest.
type serviceProcess struct {
	cmd  *exec.Cmd
	logs *ringLog
	env  map[string]string
	port int
	// done is closed by the single goroutine that waits for the process, so a
	// restart never races the reaper by calling Wait twice.
	done    chan struct{}
	started time.Time
}

// ringLog keeps the last N lines of a service output. A guest cannot grow a log
// file without bound, so the tail is bounded by construction.
type ringLog struct {
	mu     sync.Mutex
	lines  []string
	max    int
	offset int
}

func newRingLog(max int) *ringLog { return &ringLog{max: max} }

func (r *ringLog) write(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
	r.offset += len(line) + 1
}

func (r *ringLog) read() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func main() {
	port := envInt("AGENT_PORT", 51000)
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = configPath
	}

	a := &agent{procs: map[string]*serviceProcess{}, started: time.Now().UTC(), configPath: path}
	config, err := loadConfig(path)
	if err != nil {
		// A guest without a configuration is a provisioning bug: fail loudly
		// instead of serving an empty environment that looks healthy.
		log.Fatalf("load guest config: %v", err)
	}
	a.mu.Lock()
	a.config = config
	a.mu.Unlock()

	for _, service := range config.Services {
		if err := a.start(service); err != nil {
			log.Printf("start service %s: %v", service.Name, err)
		}
	}

	listener, err := vsockListener(port)
	if err != nil {
		log.Fatalf("listen on vsock %d: %v", port, err)
	}
	log.Printf("guest agent listening on vsock port %d with %d services", port, len(config.Services))
	server := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := server.Serve(listener); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// configPathOrDefault resolves where the live configuration is persisted.
func (a *agent) configPathOrDefault() string {
	if a.configPath != "" {
		return a.configPath
	}
	return configPath
}

func (a *agent) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/exec", a.handleExec)
	mux.HandleFunc("/logs", a.handleLogs)
	mux.HandleFunc("/metrics", a.handleMetrics)
	mux.HandleFunc("/env", a.handleEnv)
	mux.HandleFunc("/restart", a.handleRestart)
	mux.HandleFunc("/put-file", a.handlePutFile)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "guest agent ok")
	})
	return mux
}

// handlePutFile atomically installs a binary or static files supplied by the
// host adapter, then restarts the requested service. It deliberately accepts
// no arbitrary destination paths: files may only replace the service config
// under /etc/app or an agent-owned path under /run/incidentsim.
func (a *agent) handlePutFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Service string            `json:"service"`
		Kind    string            `json:"kind"`
		Binary  []byte            `json:"binary"`
		Files   map[string][]byte `json:"files"`
		Restart bool              `json:"restart"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.Service == "" || (request.Kind != "binary" && request.Kind != "files") {
		http.Error(w, "service and supported artifact kind are required", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	index := -1
	for i := range a.config.Services {
		if a.config.Services[i].Name == request.Service {
			index = i
			break
		}
	}
	if index < 0 {
		http.Error(w, "unknown service "+request.Service, http.StatusNotFound)
		return
	}
	if request.Kind == "binary" {
		if len(request.Binary) == 0 {
			http.Error(w, "binary is empty", http.StatusBadRequest)
			return
		}
		path := filepath.Join("/run/incidentsim/artifacts", request.Service)
		if err := atomicWrite(path, request.Binary, 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.config.Services[index].Command = []string{path}
	} else {
		for name, content := range request.Files {
			clean := filepath.Clean(name)
			if !strings.HasPrefix(clean, "/etc/app/") && !strings.HasPrefix(clean, "/run/incidentsim/") {
				http.Error(w, "invalid artifact file "+name, http.StatusBadRequest)
				return
			}
			if err := atomicWrite(clean, content, 0o600); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	if err := saveConfig(a.configPathOrDefault(), a.config); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if request.Restart {
		if err := a.startLocked(a.config.Services[index]); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]any{"deployed": request.Service})
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// handleExec runs a command inside the guest. It is the only way the core can
// reach a service, which is why it is the only write-ish endpoint here.
func (a *agent) handleExec(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Service string            `json:"service"`
		Command []string          `json:"command"`
		Env     map[string]string `json:"env,omitempty"`
		Timeout int               `json:"timeout_ms,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(request.Command) == 0 {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}

	timeout := 30 * time.Second
	if request.Timeout > 0 {
		timeout = time.Duration(request.Timeout) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, request.Command[0], request.Command[1:]...)
	cmd.Env = os.Environ()
	for key, value := range request.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			exitCode = exitErr.ExitCode()
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]any{
		"exit_code": exitCode,
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
	})
}

// handleEnv reports the environment a service is running with, which is how a
// policy check sees whether a masked symptom is still masked.
func (a *agent) handleEnv(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	a.mu.Lock()
	proc, ok := a.procs[service]
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown service "+service, http.StatusNotFound)
		return
	}
	writeJSON(w, proc.env)
}

// handleRestart restarts a service with a patched environment. The config file
// is updated first, because that is what makes the change survive a reset.
func (a *agent) handleRestart(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Service string            `json:"service"`
		Env     map[string]string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.applyEnv(request.Service, request.Env); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"restarted": request.Service})
}

func (a *agent) applyEnv(service string, patch map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := -1
	for i := range a.config.Services {
		if a.config.Services[i].Name == service {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("unknown service %q", service)
	}
	if a.config.Services[index].Env == nil {
		a.config.Services[index].Env = map[string]string{}
	}
	for key, value := range patch {
		if value == "" {
			delete(a.config.Services[index].Env, key)
			continue
		}
		a.config.Services[index].Env[key] = value
	}
	if err := saveConfig(a.configPathOrDefault(), a.config); err != nil {
		return err
	}
	return a.startLocked(a.config.Services[index])
}

// handleLogs returns the tail of a service output.
func (a *agent) handleLogs(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	tail := 200
	if value := r.URL.Query().Get("tail"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			tail = parsed
		}
	}
	a.mu.Lock()
	proc, ok := a.procs[service]
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown service "+service, http.StatusNotFound)
		return
	}
	lines := proc.logs.read()
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	writeJSON(w, map[string]any{"service": service, "lines": lines})
}

// handleMetrics proxies the service's own metrics endpoint. The adapter scrapes
// the guest endpoint, not the host, because in a microVM the service is not
// reachable from outside.
func (a *agent) handleMetrics(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	a.mu.Lock()
	proc, ok := a.procs[service]
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown service "+service, http.StatusNotFound)
		return
	}
	if proc.port <= 0 {
		http.Error(w, "service has no metrics port", http.StatusNotFound)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", proc.port), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "scrape service metrics: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("service metrics status %d", resp.StatusCode), http.StatusBadGateway)
		return
	}
	metrics, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"service": service, "metrics": string(metrics), "started_at": proc.started})
}

func (a *agent) start(service serviceConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.startLocked(service)
}

func (a *agent) startLocked(service serviceConfig) error {
	if proc, ok := a.procs[service.Name]; ok && proc.cmd != nil && proc.cmd.Process != nil {
		_ = proc.cmd.Process.Kill()
		// Reaping happens in the reaper goroutine, so waiting for its channel
		// is the safe way to make sure the pid is gone before restarting.
		<-proc.done
	}
	proc := &serviceProcess{
		logs:    newRingLog(5000),
		env:     map[string]string{},
		port:    service.Port,
		done:    make(chan struct{}),
		started: time.Now().UTC(),
	}
	for key, value := range service.Env {
		proc.env[key] = value
	}
	command := service.Command
	if len(service.Entrypoint) > 0 {
		command = append(append([]string(nil), service.Entrypoint...), command...)
	}
	if len(command) == 0 {
		command = []string{"/usr/local/bin/incidentsim-workload"}
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = os.Environ()
	for key, value := range proc.env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdout = lineWriter{proc.logs, "stdout"}
	cmd.Stderr = lineWriter{proc.logs, "stderr"}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", service.Name, err)
	}
	proc.cmd = cmd
	a.procs[service.Name] = proc
	// Exactly one goroutine waits for the process: a restart that also waited
	// would race the reaper over cmd.Wait.
	go func() {
		defer close(proc.done)
		_ = cmd.Wait()
		proc.logs.write("service exited")
	}()
	return nil
}

// lineWriter splits process output into log lines.
type lineWriter struct {
	logs   *ringLog
	stream string
}

func (l lineWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if strings.TrimSpace(line) != "" {
			l.logs.write(l.stream + " " + line)
		}
	}
	return len(p), nil
}

func loadConfig(path string) (guestConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return guestConfig{}, err
	}
	var config guestConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return guestConfig{}, err
	}
	return config, nil
}

func saveConfig(path string, config guestConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(body)
}

// vsockListener opens the AF_VSOCK port the adapter connects to. The guest id
// comes from the kernel command line the adapter sets.
func vsockListener(port int) (net.Listener, error) {
	return listenVsock(uint32(port))
}

func asExitError(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError)
	if ok {
		*target = exitErr
	}
	return ok
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return fallback
}
