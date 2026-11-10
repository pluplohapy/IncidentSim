package core

import (
	"context"
	"time"
)

// AuditRecorder is what the core needs from a session-audit backend.
//
// It is declared here rather than in the audit package so the core has no
// dependency on an optional integration: a stub, a Teleport uploader or a
// custom backend all satisfy this one method.
type AuditRecorder interface {
	// Record stores one session. It must not be on the critical path.
	Record(ctx context.Context, record SessionRecord) error
	// Name identifies the backend in logs and in the journal.
	Name() string
}

// SessionRecord is the audit entry of one task session.
type SessionRecord struct {
	SessionID string
	TaskID    string
	StartedAt time.Time
	EndedAt   time.Time
}
