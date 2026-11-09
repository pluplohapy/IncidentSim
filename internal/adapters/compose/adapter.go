package compose

import (
	"context"
	"errors"
	"fmt"
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
	id        string
	docker    string
	workdir   string
	client    *Client
	spec      runtime.Spec
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
		spec:      spec,
		hostPorts: map[string]int{},
	}
	project.client = project.newClient()
	a.sessions[sessionID] = project
	a.mu.Unlock()

	if err := os.MkdirAll(project.client.ProjectDir, 0o755); err != nil {
		return nil, fmt.Errorf("create project dir: %w", err)
	}
	document := Project(projectName(sessionID), spec)
	if err := os.WriteFile(filepath.Join(project.client.ProjectDir, composeFile), []byte(document), 0o600); err != nil {
		return nil, fmt.Errorf("write compose file: %w", err)
	}
	if err := project.client.Available(ctx); err != nil {
		return nil, err
	}
	if err := project.client.Up(ctx); err != nil {
		return nil, err
	}
	hostPorts, err := a.resolvePorts(ctx, project)
	if err != nil {
		return nil, err
	}
	project.hostPorts = hostPorts
	project.startedAt = time.Now()

	instance := a.describe(project)
	if err := runtime.WaitForHealth(ctx, instance, runtime.DefaultUpTimeout); err != nil {
		return nil, err
	}
	return instance, nil
}

// resolvePorts asks Docker which host port each service port landed on.
func (a *Adapter) resolvePorts(ctx context.Context, project *session) (map[string]int, error) {
	out := map[string]int{}
	for _, service := range project.spec.Services {
		for _, port := range service.Ports {
			published, err := project.client.Port(ctx, service.Name, port)
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
	return out, nil
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

// Exec implements runtime.Adapter.
func (a *Adapter) Exec(ctx context.Context, sessionID string, req runtime.ExecRequest) (runtime.ExecResult, error) {
	project, err := a.session(sessionID)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	start := time.Now()
	out, err := project.client.Exec(ctx, req.Service, req.Command)
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
	endpoint := project.endpoint(service)
	if endpoint == "" {
		return nil, fmt.Errorf("%w: service %s has no published port", runtime.ErrNotFound, service)
	}
	return scrapeMetrics(ctx, "http://"+endpoint+"/metrics", service)
}

// Reset implements runtime.Adapter: compose has no snapshot, so a reset is a
// down/up cycle that re-applies the declared fault.
func (a *Adapter) Reset(ctx context.Context, sessionID string) error {
	project, err := a.session(sessionID)
	if err != nil {
		return err
	}
	if err := project.client.Down(ctx); err != nil {
		return err
	}
	if err := project.client.Up(ctx); err != nil {
		return err
	}
	if _, err := a.resolvePorts(ctx, project); err != nil {
		return err
	}
	project.startedAt = time.Now()
	instance := a.describe(project)
	return runtime.WaitForHealth(ctx, instance, runtime.DefaultResetTimeout)
}

// Apply implements runtime.Adapter: the participant's fix is a config change
// plus a restart.
func (a *Adapter) Apply(ctx context.Context, sessionID string, patch runtime.EnvPatch) error {
	project, err := a.session(sessionID)
	if err != nil {
		return err
	}
	for key, value := range patch.Vars {
		env := project.envFor(patch.Service)
		if env == nil {
			return fmt.Errorf("%w: unknown service %q", runtime.ErrNotFound, patch.Service)
		}
		if value == "" {
			delete(env, key)
			continue
		}
		env[key] = value
	}
	if len(patch.Command) > 0 {
		if _, err := project.client.Exec(ctx, patch.Service, patch.Command); err != nil {
			return err
		}
	}
	if patch.Restart {
		if err := project.client.Restart(ctx, patch.Service); err != nil {
			return err
		}
	}
	// The compose file is rewritten so a later reset restores the fixed state
	// instead of silently resurrecting the fault.
	return a.writeComposeFile(project)
}

func (a *Adapter) writeComposeFile(project *session) error {
	document := Project(projectName(project.id), project.spec)
	return os.WriteFile(filepath.Join(project.client.ProjectDir, composeFile), []byte(document), 0o600)
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
