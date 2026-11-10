package core

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"incidentsim/internal/runtime"
)

// apiError is the JSON error envelope of the core API.
type apiError struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

// Server exposes the orchestrator over HTTP. It is a thin transport layer:
// every handler parses input, calls the orchestrator and serialises the
// result, so the CLI and any other client share the same behaviour.
type Server struct {
	Orchestrator *Orchestrator
	// Clock is indirected for tests.
	Clock func() time.Time
}

// NewServer returns an HTTP handler for an orchestrator.
func NewServer(orchestrator *Orchestrator) *Server {
	return &Server{Orchestrator: orchestrator, Clock: time.Now}
}

// Handler builds the route table of the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/v1/tasks", s.handleTasks)
	mux.HandleFunc("/api/v1/tasks/", s.handleTaskDetail)
	mux.HandleFunc("/api/v1/sessions", s.handleSessions)
	mux.HandleFunc("/api/v1/sessions/", s.handleSession)
	return logging(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"engine":   s.Orchestrator.Config.Engine,
		"tasks":    len(s.Orchestrator.Catalog.IDs()),
		"sessions": len(s.Orchestrator.Sessions.List()),
	})
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": s.Orchestrator.Catalog.List()})
}

func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id required", "")
		return
	}
	task, err := s.Orchestrator.Catalog.Load(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         task.Metadata.ID,
		"title":      task.Metadata.Title,
		"level":      task.Metadata.Level,
		"symptoms":   task.Spec.Symptoms,
		"root_cause": task.Spec.RootCause.Summary,
		"theory":     task.Spec.Theory,
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		views := make([]SessionView, 0)
		for _, session := range s.Orchestrator.Sessions.List() {
			views = append(views, session.Snapshot())
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": views})
	case http.MethodPost:
		var body struct {
			TaskID string `json:"task_id"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
			return
		}
		if body.TaskID == "" {
			writeError(w, http.StatusBadRequest, "task_id is required", "")
			return
		}
		session, err := s.Orchestrator.Start(r.Context(), body.TaskID)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), sessionStateDetail(session))
			return
		}
		writeJSON(w, http.StatusCreated, session.Snapshot())
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
	}
}

func sessionStateDetail(session *Session) string {
	if session == nil || session.journal == nil {
		return ""
	}
	return session.journal.Summarise()
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")
	if rest == "" {
		s.handleSessions(w, r)
		return
	}
	id, action, _ := strings.Cut(rest, "/")
	session, err := s.Orchestrator.Sessions.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "")
		return
	}

	switch action {
	case "":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"session": session.Snapshot(),
			"journal": session.journal.Events(),
		})
	case "metrics":
		s.handleMetrics(w, r, session)
	case "logs":
		s.handleLogs(w, r, session)
	case "hint":
		s.handleHint(w, r, session)
	case "apply":
		s.handleApply(w, r, session)
	case "exec":
		s.handleExec(w, r, session)
	case "verify":
		s.handleVerify(w, r, session)
	case "reset":
		s.handleReset(w, r, session)
	case "postmortem":
		s.handlePostmortem(w, r, session)
	case "stop":
		s.handleStop(w, r, session)
	default:
		writeError(w, http.StatusNotFound, "unknown action "+action, "")
	}
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	service := r.URL.Query().Get("service")
	samples, err := s.Orchestrator.Metrics(r.Context(), session.ID, service)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"metrics": samples})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	opts := runtime.LogOptions{
		Service: r.URL.Query().Get("service"),
		Grep:    r.URL.Query().Get("grep"),
		Tail:    atoiDefault(r.URL.Query().Get("tail"), 200),
	}
	lines, err := s.Orchestrator.Logs(r.Context(), session.ID, opts.Service, opts)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": lines})
}

func (s *Server) handleHint(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	level := atoiDefault(r.URL.Query().Get("level"), 0)
	hint, found, err := s.Orchestrator.Hint(session.ID, level)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hint": hint, "found": found})
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	var body struct {
		Service string            `json:"service"`
		Vars    map[string]string `json:"vars"`
		Restart *bool             `json:"restart"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	restart := true
	if body.Restart != nil {
		restart = *body.Restart
	}
	if len(body.Vars) == 0 {
		writeError(w, http.StatusBadRequest, "vars are required: a fix must change at least one variable", "")
		return
	}
	patch := runtime.EnvPatch{Service: body.Service, Vars: body.Vars, Restart: restart}
	if err := s.Orchestrator.Apply(r.Context(), session.ID, patch); err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": patch})
}

// handleExec runs a statement on a database service. It is the participant's
// write path for db-level tasks, where the fix is a statement rather than an
// environment variable.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	var body struct {
		Service   string `json:"service"`
		Statement string `json:"statement"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if body.Statement == "" {
		writeError(w, http.StatusBadRequest, "statement is required", "")
		return
	}
	result, err := s.Orchestrator.ExecSQL(r.Context(), session.ID, body.Service, body.Statement)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), result.Stderr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exit_code": result.ExitCode,
		"stdout":    result.Stdout,
		"stderr":    result.Stderr,
	})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	report, err := s.Orchestrator.Verify(r.Context(), session.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	if err := s.Orchestrator.Reset(r.Context(), session.ID); err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

// handlePostmortem returns the generated draft of a session report.
func (s *Server) handlePostmortem(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	postmortem, err := s.Orchestrator.GeneratePostmortem(session.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "")
		return
	}
	if format := r.URL.Query().Get("format"); format == "md" || format == "markdown" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, postmortem.Render())
		return
	}
	writeJSON(w, http.StatusOK, postmortem)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	if err := s.Orchestrator.Stop(r.Context(), session.ID); err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true})
}

// writeJSON answers with a JSON body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message, details string) {
	writeJSON(w, status, apiError{Error: message, Details: details})
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(target)
}

func atoiDefault(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// logging is a minimal request logger: it records the method, the path and the
// duration of every request so a session can be correlated with the API calls
// that produced it.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
