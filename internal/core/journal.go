package core

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"incidentsim/internal/runtime"
)

// Event types recorded in a session journal. They are a closed set: the
// postmortem generator keys on them, so a new type is a change of the
// postmortem contract, not a free-form tag.
const (
	EventSessionStarted  = "session.started"
	EventEnvironmentUp   = "environment.up"
	EventFaultActivated  = "fault.activated"
	EventFaultFailed     = "fault.failed"
	EventHintRequested   = "hint.requested"
	EventFixApplied      = "fix.applied"
	EventFixFailed       = "fix.failed"
	EventVerifyRun       = "verify.run"
	EventReset           = "environment.reset"
	EventTeardown        = "environment.down"
	EventSessionFinished = "session.finished"

	// Code-level lifecycle events.
	EventApplyStarted     = "apply.started"
	EventBuildOK          = "build.ok"
	EventBuildFailed      = "build.failed"
	EventServiceRecreated = "service.recreated"

	// Shell-exec events: a participant debugging a session runs commands
	// inside the environment, and the journal is the audit trail of that.
	EventExecRun    = "exec.run"
	EventExecFailed = "exec.failed"
)

// Event is one line of a session journal.
type Event struct {
	// Seq is the per-journal sequence number, starting at 1.
	Seq int `json:"seq"`
	// At is the event time in UTC.
	At time.Time `json:"at"`
	// Type is one of the Event* constants.
	Type string `json:"type"`
	// Message is a human readable summary.
	Message string `json:"message"`
	// Data carries structured detail, e.g. the patch that was applied.
	Data map[string]any `json:"data,omitempty"`
}

// Journal is the append-only event log of one session.
//
// It is the source of the postmortem and of the audit trail: everything the
// generator needs must be here, which is why writes go through Record rather
// than through a generic logger.
type Journal struct {
	mu        sync.Mutex
	path      string
	seq       int
	events    []Event
	file      *os.File
	persist   bool
	sessionID string
}

// NewJournal returns an in-memory journal. Tests and ephemeral sessions use it.
func NewJournal(sessionID string) *Journal {
	return &Journal{sessionID: sessionID}
}

// OpenJournal opens (or creates) the journal file of a session under dir.
func OpenJournal(dir, sessionID string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create journal dir: %w", err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	journal := &Journal{path: path, persist: true, sessionID: sessionID}
	if existing, err := readJournalFile(path); err == nil {
		journal.events = existing
		journal.seq = len(existing)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	journal.file = file
	return journal, nil
}

// SessionID returns the session this journal belongs to.
func (j *Journal) SessionID() string { return j.sessionID }

// Record appends an event. It never fails the caller: a journal write problem
// must not abort a diagnosis session, but it is reported through the returned
// error so the API can tell the operator.
func (j *Journal) Record(eventType, message string, data map[string]any) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.seq++
	event := Event{Seq: j.seq, At: time.Now().UTC(), Type: eventType, Message: message, Data: data}
	j.events = append(j.events, event)
	if !j.persist {
		return nil
	}
	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	if _, err := j.file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return j.file.Sync()
}

// Events returns a copy of the recorded events in order.
func (j *Journal) Events() []Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Event, len(j.events))
	copy(out, j.events)
	return out
}

// Since returns the events of a type that happened after a timestamp.
func (j *Journal) Since(after time.Time) []Event {
	var out []Event
	for _, event := range j.Events() {
		if event.At.After(after) {
			out = append(out, event)
		}
	}
	return out
}

// Path returns the journal file path, or "" for an in-memory journal.
func (j *Journal) Path() string { return j.path }

// Close flushes and closes the journal file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}

// Summarise renders the journal for logs and for the CLI postmortem output.
func (j *Journal) Summarise() string {
	events := j.Events()
	lines := make([]string, 0, len(events))
	for _, event := range events {
		lines = append(lines, fmt.Sprintf("%s %-18s %s", event.At.Format(time.RFC3339), event.Type, event.Message))
	}
	return strings.Join(lines, "\n")
}

func readJournalFile(path string) ([]Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var events []Event
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue // a torn last line is not a reason to lose the journal
		}
		events = append(events, event)
	}
	return events, scanner.Err()
}

// LoadJournal reads a journal file for the postmortem generator.
func LoadJournal(dir, sessionID string) (*Journal, error) {
	path := filepath.Join(dir, sessionID+".jsonl")
	events, err := readJournalFile(path)
	if err != nil {
		return nil, fmt.Errorf("read journal %s: %w", path, err)
	}
	return &Journal{path: path, events: events, seq: len(events), sessionID: sessionID}, nil
}

// ListJournals returns the session ids that have a journal, newest first.
func ListJournals(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type entry struct {
		id    string
		mtime time.Time
	}
	var found []entry
	for _, item := range entries {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".jsonl") {
			continue
		}
		info, err := item.Info()
		if err != nil {
			continue
		}
		found = append(found, entry{
			id:    strings.TrimSuffix(item.Name(), ".jsonl"),
			mtime: info.ModTime(),
		})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].mtime.After(found[j].mtime) })
	ids := make([]string, 0, len(found))
	for _, item := range found {
		ids = append(ids, item.id)
	}
	return ids, nil
}

// instanceToData projects a runtime instance into journal-safe data.
func instanceToData(instance *runtime.Instance) map[string]any {
	if instance == nil {
		return nil
	}
	services := make([]string, 0, len(instance.Services))
	for _, service := range instance.Services {
		services = append(services, service.Name)
	}
	return map[string]any{
		"engine":   instance.Engine,
		"services": services,
		"workdir":  instance.Workdir,
	}
}
