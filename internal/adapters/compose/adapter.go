package compose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"incidentsim/internal/runtime"
)

// Adapter implements runtime.Adapter with Docker Compose. It is the fallback
// engine used whenever KVM or Firecracker is unavailable, and the engine the
// test-suite runs against.
type Adapter struct {
	// BaseDir is where per-session compose projects are written, e.g.
	// ".incidentsim/compose".
	BaseDir string
	// Docker is the docker binary, overridable in tests.
	Docker string

	mu       sync.Mutex
	sessions map[string]*session
}

// session is one compose project of one task run.
type session struct {
	id      string
	docker  string
	workdir string
	client  *Client
	// spec is the live environment: Apply mutates it as the participant
	// fixes the task. baseline never changes, so Reset can restore the
	// declared fault instead of whatever the participant left behind.
	spec      runtime.Spec
	baseline  runtime.Spec
	hostPorts map[string]int
	startedAt time.Time
}

// nameRe fixes the characters allowed in a compose project name.
var nameRe = regexp.MustCompile(`[^a-z0-9-]+`)

// New returns an adapter that writes projects under baseDir.
func New(baseDir string) *Adapter {
	return &Adapter{BaseDir: baseDir, Docker: "docker", sessions: map[string]*session{}}
}

// Name implements runtime.Adapter.
func (a *Adapter) Name() string { return "compose" }

// Up implements runtime.Adapter.
func (a *Adapter) Up(ctx context.Context, sessionID string, spec runtime.Spec) (*runtime.Instance, error) {
	a.mu.Lock()
	if existing, ok := a.sessions[sessionID]; ok {
		a.mu.Unlock()
		return a.describe(existing), nil
	}
	project := &session{
		id:        sessionID,
		docker:    a.Docker,
		workdir:   a.projectDir(sessionID),
		spec:      cloneSpec(spec),
		baseline:  cloneSpec(spec),
		hostPorts: map[string]int{},
	}
	project.client = project.newClient()
	a.sessions[sessionID] = project
	a.mu.Unlock()

	if err := os.MkdirAll(project.client.ProjectDir, 0o755); err != nil {
		return nil, fmt.Errorf("create project dir: %w", err)
	}
	if err := writeServiceConfig(project.client.ProjectDir, spec); err != nil {
		return nil, err
	}
	document := Project(project.client.ProjectDir, projectName(sessionID), spec)
	if err := os.WriteFile(filepath.Join(project.client.ProjectDir, composeFile), []byte(document), 0o600); err != nil {
		return nil, fmt.Errorf("write compose file: %w", err)
	}
	if err := project.client.Available(ctx); err != nil {
		return nil, err
	}
	if err := project.client.Up(ctx); err != nil {
		return nil, err
	}
	if err := a.requireRunning(ctx, project); err != nil {
		return nil, err
	}
	if _, err := a.resolvePorts(ctx, project); err != nil {
		return nil, err
	}
	project.startedAt = time.Now()

	instance := a.describe(project)
	if err := runtime.WaitForHealth(ctx, instance, runtime.DefaultUpTimeout); err != nil {
		return nil, err
	}
	return instance, nil
}

// requireRunning verifies that every declared service is actually running. A
// service that exits at boot (a bad image, a missing password) must fail the
// session here with the service named, instead of surfacing later as an
// unreachable endpoint.
func (a *Adapter) requireRunning(ctx context.Context, project *session) error {
	running, err := project.client.RunningServices(ctx)
	if err != nil {
		return err
	}
	up := map[string]bool{}
	for _, name := range running {
		up[name] = true
	}
	var stalled []string
	for _, declared := range project.spec.Services {
		if !up[declared.Name] {
			stalled = append(stalled, declared.Name)
		}
	}
	if len(stalled) == 0 {
		return nil
	}
	return fmt.Errorf("services %v are not running: check `incidentsim logs --service %s`",
		stalled, stalled[0])
}

// resolvePorts asks Docker which host port each service port landed on and
// stores the mapping on the session, so every later describe reports the
// effect of the resolution rather than the state before it.
//
// A freshly recreated container is not reachable through `compose port` until
// Docker has finished attaching its networking, so a missing mapping is
// retried briefly instead of being reported as "no published port".
func (a *Adapter) resolvePorts(ctx context.Context, project *session) (map[string]int, error) {
	out := map[string]int{}
	for _, service := range project.spec.Services {
		for _, port := range service.Ports {
			published, err := a.publishedPort(ctx, project, service.Name, port)
			if err != nil {
				continue // a service may legitimately not publish its port
			}
			host, err := hostPort(published)
			if err != nil {
				return nil, err
			}
			out[fmt.Sprintf("%s:%d", service.Name, port)] = host
		}
	}
	project.hostPorts = out
	return out, nil
}

// publishedPort polls `compose port` until it answers or the deadline passes.
func (a *Adapter) publishedPort(ctx context.Context, project *session, service string, port int) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		published, err := project.client.Port(ctx, service, port)
		if err == nil {
			return published, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return "", lastErr
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// hostPort parses "127.0.0.1:32768" or "[::1]:32768".
func hostPort(published string) (int, error) {
	trimmed := strings.TrimSpace(published)
	idx := strings.LastIndex(trimmed, ":")
	if idx < 0 {
		return 0, fmt.Errorf("cannot parse published port %q", published)
	}
	port, err := strconv.Atoi(trimmed[idx+1:])
	if err != nil {
		return 0, fmt.Errorf("cannot parse published port %q: %w", published, err)
	}
	return port, nil
}

// Down implements runtime.Adapter.
func (a *Adapter) Down(ctx context.Context, sessionID string) error {
	project, err := a.session(sessionID)
	if err != nil {
		if errors.Is(err, runtime.ErrNotFound) {
			return nil
		}
		return err
	}
	downErr := project.client.Down(ctx)
	a.mu.Lock()
	delete(a.sessions, sessionID)
	a.mu.Unlock()
	return downErr
}

// Exec implements runtime.Adapter. The command is whatever the caller asked
// for: a shell pipeline for a participant debugging a session, `env` for a
// policy check, or a client binary for a statement.
func (a *Adapter) Exec(ctx context.Context, sessionID string, req runtime.ExecRequest) (runtime.ExecResult, error) {
	project, err := a.session(sessionID)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	start := time.Now()
	var out commandOutput
	if req.TTY {
		out, err = project.client.ExecTerminal(ctx, req.Service, req.Command)
	} else {
		out, err = project.client.Exec(ctx, req.Service, req.Command)
	}
	result := runtime.ExecResult{
		Stdout:   out.stdout,
		Stderr:   out.stderr,
		Duration: time.Since(start),
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result, nil // a non-zero exit is a result, not a failure
		}
		return result, err
	}
	return result, nil
}

// Logs implements runtime.Adapter.
func (a *Adapter) Logs(ctx context.Context, sessionID string, opts runtime.LogOptions) ([]runtime.LogLine, error) {
	project, err := a.session(sessionID)
	if err != nil {
		return nil, err
	}
	out, err := project.client.Logs(ctx, opts.Service, opts.Tail)
	if err != nil {
		return nil, err
	}
	return parseLogLines(out.stdout, opts), nil
}

// Metrics implements runtime.Adapter. Compose services expose Prometheus text
// on /metrics, which the core scrapes through the published port.
func (a *Adapter) Metrics(ctx context.Context, sessionID string, service string) ([]runtime.MetricSample, error) {
	project, err := a.session(sessionID)
	if err != nil {
		return nil, err
	}
	if service == "" {
		// An empty service means "everything the environment reports", which
		// is what the CLI does by default.
		var samples []runtime.MetricSample
		for _, declared := range project.spec.Services {
			collected, err := a.metricsOf(ctx, project, declared.Name)
			if err != nil {
				continue
			}
			samples = append(samples, collected...)
		}
		return samples, nil
	}
	return a.metricsOf(ctx, project, service)
}

func (a *Adapter) metricsOf(ctx context.Context, project *session, service string) ([]runtime.MetricSample, error) {
	endpoint := project.endpoint(service)
	if endpoint == "" {
		return nil, fmt.Errorf("%w: service %s has no published port", runtime.ErrNotFound, service)
	}
	return scrapeMetrics(ctx, "http://"+endpoint+"/metrics", service)
}

// Reset implements runtime.Adapter: compose has no snapshot, so a reset is a
// down/up cycle that re-renders the declared manifest, fault included. The
// live spec is discarded, which is what makes a reset a reset rather than a
// restart of the fixed environment.
func (a *Adapter) Reset(ctx context.Context, sessionID string) (*runtime.Instance, error) {
	project, err := a.session(sessionID)
	if err != nil {
		return nil, err
	}
	project.spec = cloneSpec(project.baseline)
	if err := a.writeComposeFile(project); err != nil {
		return nil, err
	}
	if err := project.client.Down(ctx); err != nil {
		return nil, err
	}
	if err := project.client.Up(ctx); err != nil {
		return nil, err
	}
	if err := a.requireRunning(ctx, project); err != nil {
		return nil, err
	}
	if _, err := a.resolvePorts(ctx, project); err != nil {
		return nil, err
	}
	project.startedAt = time.Now()
	instance := a.describe(project)
	if err := runtime.WaitForHealth(ctx, instance, runtime.DefaultResetTimeout); err != nil {
		return nil, err
	}
	return instance, nil
}

// Apply implements runtime.Adapter: the participant's fix is a config change
// plus a service recreate. It returns the refreshed instance because the
// recreate can move the published port.
func (a *Adapter) Apply(ctx context.Context, sessionID string, patch runtime.EnvPatch) (*runtime.Instance, error) {
	project, err := a.session(sessionID)
	if err != nil {
		return nil, err
	}
	for key, value := range patch.Vars {
		env := project.envFor(patch.Service)
		if env == nil {
			return nil, fmt.Errorf("%w: unknown service %q", runtime.ErrNotFound, patch.Service)
		}
		if value == "" {
			delete(env, key)
			continue
		}
		env[key] = value
	}
	// Sync the generated config file so env changes (e.g. LOG_LEVEL=info)
	// are reflected in the file mounted into the container. The service
	// reads this file at startup, so a stale config file would mask the fix.
	if err := writeServiceConfig(project.client.ProjectDir, project.spec); err != nil {
		return nil, fmt.Errorf("sync config file: %w", err)
	}
	// Files are written to the host filesystem inside the compose project
	// directory: the config file is mounted into the container, so writing it
	// here and restarting the service is a real fix - the participant edits
	// the config file on disk, exactly like on a production host.
	for patchPath, content := range patch.Files {
		hostPath := filepath.Join(project.client.ProjectDir, "config", filepath.Base(patchPath))
		if err := a.writeConfigFile(project, patch.Service, hostPath, content); err != nil {
			return nil, fmt.Errorf("write %s into %s: %w", patchPath, patch.Service, err)
		}
	}
	// If the patch carries a new image tag (e.g. after rebuilding the
	// participant's workspace), update the service image in the live spec
	// so the recreated container uses the fixed code.
	if patch.Image != "" {
		for i := range project.spec.Services {
			if project.spec.Services[i].Name == patch.Service {
				project.spec.Services[i].Image = patch.Image
				break
			}
		}
	}
	// The compose file is rewritten before the service is recreated: compose
	// builds the new container from the file on disk, so writing it afterwards
	// would recreate the service with the old configuration and the fix would
	// appear to do nothing.
	if err := a.writeComposeFile(project); err != nil {
		return nil, err
	}
	if len(patch.Command) > 0 {
		if _, err := project.client.Exec(ctx, patch.Service, patch.Command); err != nil {
			return nil, err
		}
	}
	if patch.Restart {
		if err := project.client.Restart(ctx, patch.Service); err != nil {
			return nil, err
		}
	}
	// Ports are re-resolved after the recreate: compose may hand the new
	// container a different ephemeral host port, and a verifier probing the
	// old one would call the environment broken instead of fixed.
	if err := a.resolveVerifiedPorts(ctx, project); err != nil {
		return nil, err
	}
	return a.describe(project), nil
}

// resolveVerifiedPorts resolves ports and confirms each endpoint answers. A
// recreate has a window in which `compose port` still reports the port of the
// container being replaced, so a mapping is only accepted once the service
// really answers requests through it.
func (a *Adapter) resolveVerifiedPorts(ctx context.Context, project *session) error {
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 1; ; attempt++ {
		if _, err := a.resolvePorts(ctx, project); err != nil {
			return err
		}
		if a.endpointsAnswer(ctx, project) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("endpoints of session %s did not come up", project.id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (a *Adapter) endpointsAnswer(ctx context.Context, project *session) bool {
	for _, service := range project.spec.Services {
		endpoint := project.endpoint(service.Name)
		if endpoint == "" {
			if debugPorts {
				log.Printf("compose: no endpoint for service %s yet", service.Name)
			}
			continue
		}
		if !serviceAnswers(ctx, endpoint, service.Healthcheck) {
			if debugPorts {
				log.Printf("compose: endpoint %s of %s does not answer", endpoint, service.Name)
			}
			return false
		}
	}
	return true
}

// serviceAnswers reports whether a service really serves requests.
//
// A bare TCP dial is not enough and is the reason a verify right after a
// recreate could fail: Docker's port proxy accepts the connection before the
// process inside the container listens, so a check that starts measuring
// immediately sees a connection reset and calls the environment broken. A
// service that declares a healthcheck is probed over HTTP instead, which is
// the only proof that it is the service answering and not the proxy.
//
// Any HTTP status counts as an answer: a degraded service that says so itself
// is up, and a verifier must be the one to grade the degradation.
func serviceAnswers(ctx context.Context, endpoint string, check *runtime.HealthcheckSpec) bool {
	if check == nil || check.Path == "" {
		conn, err := net.DialTimeout("tcp", endpoint, time.Second)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	url := fmt.Sprintf("http://%s%s", endpoint, check.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	return true
}

// debugPorts enables port-resolution diagnostics, which are otherwise invisible
// from outside the adapter.
var debugPorts = os.Getenv("INCIDENTSIM_DEBUG_PORTS") != ""

// writeComposeFile renders the live spec of a session to its compose project.
func (a *Adapter) writeComposeFile(project *session) error {
	document := Project(project.client.ProjectDir, projectName(project.id), project.spec)
	return os.WriteFile(filepath.Join(project.client.ProjectDir, composeFile), []byte(document), 0o600)
}

// writeConfigFile writes content into the config file for a service on the
// host. The file is mounted into the container, so writing it here and
// restarting the service is a real fix - the participant edits the config file
// on disk, exactly like on a production host.
func (a *Adapter) writeConfigFile(project *session, service, path, content string) error {
	// The path from the CLI is inside the container (e.g. /etc/app/app.conf).
	// Map it to the host path in the compose project's config directory.
	hostPath := filepath.Join(project.client.ProjectDir, "config", filepath.Base(path))
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(hostPath, []byte(content), 0o600); err != nil {
		return err
	}
	return nil
}

func (a *Adapter) session(sessionID string) (*session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	project, ok := a.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", runtime.ErrNotFound, sessionID)
	}
	return project, nil
}

func (a *Adapter) describe(project *session) *runtime.Instance {
	services := make([]runtime.Service, 0, len(project.spec.Services))
	for _, service := range project.spec.Services {
		endpoint := project.endpoint(service.Name)
		containerPort := 0
		if len(service.Ports) > 0 {
			containerPort = service.Ports[0]
		}
		services = append(services, runtime.Service{
			Name:          service.Name,
			Endpoint:      endpoint,
			ContainerPort: containerPort,
			Healthcheck:   service.Healthcheck,
		})
	}
	return &runtime.Instance{
		ID:        project.id,
		Engine:    "compose",
		Services:  services,
		Workdir:   project.client.ProjectDir,
		StartedAt: project.startedAt,
	}
}

func (a *Adapter) projectDir(sessionID string) string {
	return filepath.Join(a.BaseDir, "compose", projectName(sessionID))
}

func projectName(sessionID string) string {
	return "incidentsim-" + nameRe.ReplaceAllString(strings.ToLower(sessionID), "-")
}

// newClient builds the compose client of the session.
func (s *session) newClient() *Client {
	client := NewClient(s.workdir, projectName(s.id))
	if s.docker != "" {
		client.Docker = s.docker
	}
	return client
}

// cloneSpec deep copies a spec so that patching the live spec never mutates the
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
	out.FaultEnv = nil
	if spec.FaultEnv != nil {
		out.FaultEnv = make(map[string]string, len(spec.FaultEnv))
		for key, value := range spec.FaultEnv {
			out.FaultEnv[key] = value
		}
	}
	return out
}

// envFor returns the mutable environment of a declared service.
func (s *session) envFor(name string) map[string]string {
	for i := range s.spec.Services {
		if s.spec.Services[i].Name == name {
			if s.spec.Services[i].Env == nil {
				s.spec.Services[i].Env = map[string]string{}
			}
			return s.spec.Services[i].Env
		}
	}
	return nil
}

// endpoint returns the host endpoint of the first published port of a service.
func (s *session) endpoint(service string) string {
	for _, declared := range s.spec.Services {
		if declared.Name != service {
			continue
		}
		for _, port := range declared.Ports {
			if host, ok := s.hostPorts[fmt.Sprintf("%s:%d", service, port)]; ok {
				return fmt.Sprintf("127.0.0.1:%d", host)
			}
		}
	}
	return ""
}
