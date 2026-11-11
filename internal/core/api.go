package core

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"incidentsim/internal/manifest"
	"incidentsim/internal/runtime"
	"incidentsim/internal/verifier"
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
	mux.HandleFunc("/api/v1/admin/tasks/", s.handleAdminTaskDetail)
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
	// Публичный вид: только то, что участник должен знать до решения.
	// root_cause, theory, reference_solution, distractors отдаются только
	// через reveal/postmortem (после pass или give-up) и через админ-эндпоинт.
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       task.Metadata.ID,
		"title":    task.Metadata.Title,
		"level":    task.Metadata.Level,
		"symptoms": task.Spec.Symptoms,
		"editable": task.Spec.Editable,
	})
}

func (s *Server) handleAdminTaskDetail(w http.ResponseWriter, r *http.Request) {
	if !s.adminEnabled() {
		writeError(w, http.StatusForbidden, "admin endpoint requires INCIDENTSIM_ADMIN=1", "")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/tasks/")
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
		"id":                 task.Metadata.ID,
		"title":              task.Metadata.Title,
		"level":              task.Metadata.Level,
		"symptoms":           task.Spec.Symptoms,
		"editable":           task.Spec.Editable,
		"root_cause":         task.Spec.RootCause.Summary,
		"theory":             task.Spec.Theory,
		"reference":          task.Spec.Reference,
		"reference_solution": task.Spec.ReferenceSolution,
		"distractors":        task.Spec.Distractors,
	})
}

func (s *Server) adminEnabled() bool {
	return os.Getenv("INCIDENTSIM_ADMIN") == "1"
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
	// A stopped session is removed from the live registry, but its journal is
	// intentionally retained so the participant can still fetch the gated
	// postmortem after teardown.
	if action == "postmortem" && r.Method == http.MethodGet {
		if _, err := s.Orchestrator.Sessions.Get(id); err != nil {
			s.handleArchivedPostmortem(w, r, id)
			return
		}
	}
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
	case "env":
		s.handleEnv(w, r, session)
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
	case "diff":
		s.handleDiff(w, r, session)
	case "stop":
		s.handleStop(w, r, session)
	case "give-up":
		s.handleGiveUp(w, r, session)
	case "distractor":
		s.handleDistractor(w, r, session)
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

// handleEnv reports the effective environment of a service: the diagnostic
// view a participant needs before applying a fix.
func (s *Server) handleEnv(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	service := r.URL.Query().Get("service")
	env, err := s.Orchestrator.Env(r.Context(), session.ID, service)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": service, "env": env})
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
		Files   map[string]string `json:"files"`
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
	// A restart without variables or files is a legitimate action: it
	// recreates the service from the declared configuration, which is what a
	// restart distractor does and what a participant needs after editing a
	// mounted file. Only a patch that changes nothing at all is refused.
	if len(body.Vars) == 0 && len(body.Files) == 0 && !restart {
		writeError(w, http.StatusBadRequest, "vars, files or a restart are required: a fix must change at least something", "")
		return
	}
	patch := runtime.EnvPatch{Service: body.Service, Vars: body.Vars, Restart: restart, Files: body.Files}
	if err := s.Orchestrator.Apply(r.Context(), session.ID, patch); err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": patch})
}

// handleExec runs a command inside a service of a session.
//
// The command is the primitive: a participant debugging an incident needs a
// shell (cat, ps, ss, tcpdump, a manual psql session), not a fixed set of
// verbs. The statement form stays as sugar, because the client binary and the
// task database of a statement are manifest data the server owns.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	var body struct {
		Service   string   `json:"service"`
		Command   []string `json:"command"`
		Statement string   `json:"statement"`
		TimeoutMS int      `json:"timeout_ms"`
		TTY       bool     `json:"tty"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	command := body.Command
	if len(command) == 0 {
		// Sugar: a bare statement is run with the database client of the
		// task, so `--sql` does not have to duplicate manifest knowledge in
		// the CLI.
		if strings.TrimSpace(body.Statement) == "" {
			writeError(w, http.StatusBadRequest, "command is required: a list of the program and its arguments", "")
			return
		}
		command = sqlCommand(session.manifest, body.Service, body.Statement)
	}
	if len(command) == 0 {
		writeError(w, http.StatusBadRequest, "command is required: a list of the program and its arguments", "")
		return
	}
	timeout := time.Duration(body.TimeoutMS) * time.Millisecond
	result, err := s.Orchestrator.Exec(r.Context(), session.ID, runtime.ExecRequest{
		Service: body.Service,
		Command: command,
		Timeout: timeout,
		TTY:     body.TTY,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), strings.TrimSpace(result.Stderr))
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

// handlePostmortem returns the generated draft of a session report. The
// private view (root cause, reference solution) is only shown after the
// participant has passed verification or given up.
func (s *Server) handlePostmortem(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	ok, err := s.Orchestrator.canReveal(session.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "postmortem is available after a passing verification or an explicit give-up", "")
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

func (s *Server) handleArchivedPostmortem(w http.ResponseWriter, r *http.Request, sessionID string) {
	journal, err := LoadJournal(s.Orchestrator.Config.JournalDir(), sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "")
		return
	}
	allowed := false
	for _, event := range journal.Events() {
		if event.Type == EventGaveUp || (event.Type == EventVerifyRun && stringOf(event.Data["verdict"]) == verifier.VerdictPass) {
			allowed = true
			break
		}
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "postmortem is available after a passing verification or an explicit give-up", "")
		return
	}
	postmortem, err := s.Orchestrator.GeneratePostmortem(sessionID)
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

// handleGiveUp records that the participant asked for the answer. After a
// give-up the private view may be revealed.
func (s *Server) handleGiveUp(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	if err := s.Orchestrator.GiveUp(r.Context(), session.ID); err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gave_up": true})
}

func (s *Server) handleDistractor(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	var body struct {
		DistractorID string `json:"distractor_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if body.DistractorID == "" {
		writeError(w, http.StatusBadRequest, "distractor_id is required", "")
		return
	}
	task := session.manifest
	var d *manifest.Distractor
	for i := range task.Spec.Distractors {
		if task.Spec.Distractors[i].ID == body.DistractorID {
			d = &task.Spec.Distractors[i]
			break
		}
	}
	if d == nil {
		writeError(w, http.StatusNotFound, "distractor not found", body.DistractorID)
		return
	}
	failsCheck, err := s.Orchestrator.ApplyDistractor(r.Context(), session.ID, d)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"distractor": body.DistractorID, "fails_check": failsCheck})
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request, session *Session) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	diff, err := s.Orchestrator.Diff(session.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"diff": diff})
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
