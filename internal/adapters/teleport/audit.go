// Package teleport records the participant's session inside a task microVM.
//
// It is an optional integration and it is deliberately outside the main path:
// the core does not depend on it, a missing Teleport endpoint degrades to a
// stub, and no task behaviour changes when it is enabled.
package teleport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"incidentsim/internal/core"
)

// Config points at a Teleport deployment and controls what is recorded.
type Config struct {
	// Enabled turns recording on. When it is false the adapter is a no-op that
	// still satisfies the interface, so the rest of the core never branches on
	// whether audit is configured.
	Enabled bool
	// Endpoint is the Teleport auth server address, e.g.
	// teleport.example.com:3080 or https://teleport.example.com.
	Endpoint string
	// ClusterName is the Teleport cluster the recordings are uploaded to.
	ClusterName string
	// JoinToken authorises this deployment to create session recordings.
	JoinToken string
	// Timeout bounds every API call.
	Timeout time.Duration
}

// SessionRecord is one recording the deployment uploads to Teleport.
type SessionRecord struct {
	// SessionID is the IncidentSim session id.
	SessionID string `json:"session_id"`
	// TaskID is the task the session was for.
	TaskID string `json:"task_id"`
	// StartedAt and EndedAt bound the recorded window.
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	// Participants are the logins the recording attributes the actions to.
	Participants []string `json:"participants"`
	// Command is the shell command that was recorded, if any.
	Command string `json:"command,omitempty"`
	// Chunks is the captured terminal output, base64 or raw depending on the
	// Teleport version.
	Chunks []Chunk `json:"chunks,omitempty"`
}

// Chunk is one piece of a recorded terminal session.
type Chunk struct {
	At     time.Time `json:"at"`
	Stream string    `json:"stream"`
	Data   string    `json:"data"`
}

// Stub is the adapter used when Teleport is not configured. It keeps the
// recordings in memory so a deployment can still export them later.
type Stub struct {
	mu      sync.Mutex
	records []core.SessionRecord
	// Name is what logs call this backend, e.g. "teleport-stub".
	backendName string
}

// NewStub returns the no-op backend.
func NewStub() *Stub { return &Stub{backendName: "teleport-stub"} }

// Name identifies the backend in logs and in the journal.
func (s *Stub) Name() string { return s.backendName }

// Record implements the audit recorder: it keeps the record and reports success, because a
// missing audit backend must never break a diagnosis session.
func (s *Stub) Record(ctx context.Context, record core.SessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
	return nil
}

// Records returns the recordings the stub kept.
func (s *Stub) Records() []core.SessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.SessionRecord(nil), s.records...)
}

// Client is an experimental HTTP recorder. The endpoint and payload are kept
// configurable because this repository has not validated them against a real
// Teleport cluster; enabling it must therefore be treated as best effort.
type Client struct {
	Config Config
	HTTP   *http.Client
}

// New returns a Teleport-backed audit adapter.
func New(config Config) (*Client, error) {
	if config.Endpoint == "" {
		return nil, fmt.Errorf("teleport endpoint is required")
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{Config: config, HTTP: &http.Client{Timeout: timeout}}, nil
}

// Name identifies the backend in logs and in the journal.
func (c *Client) Name() string { return "teleport" }

// Record implements the audit recorder.
//
// Record sends the experimental wire payload. A failure is reported to the
// caller so the deployment can alert on it, but must be treated as a warning:
// an unverified audit integration is an audit gap, not a broken session.
func (c *Client) Record(ctx context.Context, record core.SessionRecord) error {
	payload, err := json.Marshal(toWire(record))
	if err != nil {
		return fmt.Errorf("encode recording: %w", err)
	}
	// The scheme is taken from the endpoint so a test can point at an http
	// server, and so a deployment can name an https URL in full.
	endpoint := c.Config.Endpoint
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}
	url := endpoint + "/v1/webapi/sessions/record"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Config.JoinToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.Config.JoinToken)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("teleport audit request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("teleport responded %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// toWire projects the core record into the experimental payload shape. It is
// intentionally conservative until a real Teleport deployment validates it.
func toWire(record core.SessionRecord) SessionRecord {
	return SessionRecord{
		SessionID: record.SessionID,
		TaskID:    record.TaskID,
		StartedAt: record.StartedAt,
		EndedAt:   record.EndedAt,
	}
}

// FromConfig returns the configured backend, or the stub when audit is off.
//
// The return type is the core's AuditRecorder contract: this package must not
// be imported by the core, so the boundary is the interface, not the type.
func FromConfig(cfg Config) core.AuditRecorder {
	if !cfg.Enabled {
		return NewStub()
	}
	client, err := New(cfg)
	if err != nil {
		// A misconfigured audit must degrade, not take the core down.
		return NewStub()
	}
	return client
}

// Recorder is the minimal audit contract this package satisfies. It mirrors
// core.AuditRecorder without importing the core, so the dependency points the
// right way.
type Recorder interface {
	Record(ctx context.Context, record core.SessionRecord) error
	Name() string
}
