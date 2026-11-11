package core

import (
	"fmt"
	"sync"
	"time"

	"incidentsim/internal/manifest"
	"incidentsim/internal/runtime"
)

// Session states.
const (
	StateStarting  = "starting"
	StateRunning   = "running"
	StateVerifying = "verifying"
	StateFinished  = "finished"
	StateFailed    = "failed"
	StateTornDown  = "torn_down"
)

// Session is one incident run of one participant.
type Session struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	State     string    `json:"state"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`

	mu sync.Mutex `json:"-"`

	manifest *manifest.Manifest
	instance *runtime.Instance
	journal  *Journal

	HintsUsed    int                `json:"hints_used"`
	AppliedFixes []runtime.EnvPatch `json:"applied_fixes"`
	// FaultArtifacts is what an injector changed to activate the declared
	// fault. It is kept apart from AppliedFixes because a postmortem must be
	// able to tell what the simulator broke from what the participant did.
	FaultArtifacts []runtime.EnvPatch `json:"fault_artifacts,omitempty"`
	LastVerdict    string             `json:"last_verdict,omitempty"`
	// WorkspaceDir is where the participant's editable code lives for code-level
	// tasks. It is populated at start by copying from scenarios/<task>/workspace/.
	WorkspaceDir string `json:"workspace_dir,omitempty"`
}

// Manager owns the live sessions of a core process.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewManager returns an empty session manager.
func NewManager() *Manager {
	return &Manager{sessions: map[string]*Session{}}
}

// Add registers a session.
func (m *Manager) Add(session *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[session.ID] = session
}

// Get returns a session by id.
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("unknown session %q", id)
	}
	return session, nil
}

// List returns the sessions, newest first.
func (m *Manager) List() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		out = append(out, session)
	}
	// insertion order is not preserved by a map, so sort by start time
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StartedAt.After(out[j-1].StartedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Remove drops a session. It is called after teardown so a long running core
// does not leak memory across many runs.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// SetState moves a session to a new state and stamps it.
func (s *Session) SetState(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.State = state
	s.UpdatedAt = time.Now().UTC()
}

// Snapshot returns a consistent copy of the mutable parts of a session.
func (s *Session) Snapshot() SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := SessionView{
		ID:             s.ID,
		TaskID:         s.TaskID,
		State:          s.State,
		StartedAt:      s.StartedAt,
		UpdatedAt:      s.UpdatedAt,
		HintsUsed:      s.HintsUsed,
		AppliedFixes:   append([]runtime.EnvPatch(nil), s.AppliedFixes...),
		FaultArtifacts: append([]runtime.EnvPatch(nil), s.FaultArtifacts...),
		LastVerdict:    s.LastVerdict,
		WorkspaceDir:   s.WorkspaceDir,
		Editable:       append([]string(nil), s.manifest.Spec.Editable...),
		Engine:         "",
		Services:       nil,
	}
	if s.instance != nil {
		view.Engine = s.instance.Engine
		for _, service := range s.instance.Services {
			view.Services = append(view.Services, service.Name+" ("+service.Endpoint+")")
		}
	}
	return view
}

// SessionView is the JSON projection of a session for the API and the CLI.
type SessionView struct {
	ID           string             `json:"id"`
	TaskID       string             `json:"task_id"`
	State        string             `json:"state"`
	StartedAt    time.Time          `json:"started_at"`
	UpdatedAt    time.Time          `json:"updated_at"`
	HintsUsed    int                `json:"hints_used"`
	AppliedFixes []runtime.EnvPatch `json:"applied_fixes"`
	// FaultArtifacts is what the simulator changed to create the incident.
	FaultArtifacts []runtime.EnvPatch `json:"fault_artifacts,omitempty"`
	LastVerdict    string             `json:"last_verdict,omitempty"`
	WorkspaceDir   string             `json:"workspace_dir,omitempty"`
	Editable       []string           `json:"editable,omitempty"`
	Engine         string             `json:"engine"`
	Services       []string           `json:"services,omitempty"`
}
