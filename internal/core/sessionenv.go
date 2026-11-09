package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"incidentsim/internal/runtime"
)

// sessionEnv is the injector.Environment of one session. It is the only object
// an injector gets, so the writes an injector can perform are exactly the
// writes the core is willing to record in the journal.
type sessionEnv struct {
	session *Session
	adapter runtime.Adapter
	mu      sync.Mutex
}

func (e *sessionEnv) InstanceID() string { return e.session.ID }

func (e *sessionEnv) Runtime() runtime.Adapter { return e.adapter }

// SetEnv applies an environment patch and records it as a fault artifact.
func (e *sessionEnv) SetEnv(ctx context.Context, service string, vars map[string]string) error {
	instance, err := e.adapter.Apply(ctx, e.session.ID, runtime.EnvPatch{
		Service: service,
		Vars:    vars,
		Restart: true,
	})
	if err != nil {
		return err
	}
	e.session.mu.Lock()
	e.session.instance = instance
	e.session.mu.Unlock()
	e.session.mu.Lock()
	e.session.AppliedFixes = append(e.session.AppliedFixes, runtime.EnvPatch{
		Service: service,
		Vars:    vars,
		Restart: true,
	})
	e.session.mu.Unlock()
	return nil
}

// ExecSQL runs a statement against a database service.
//
// The statement is executed through the engine's shell because the simulator
// has no database driver: a task database is reachable through the service's
// own client. This keeps the core free of a driver dependency per database.
func (e *sessionEnv) ExecSQL(ctx context.Context, service, statement string) (runtime.ExecResult, error) {
	command := []string{"gosu", "postgres", "psql", "-v", "ON_ERROR_STOP=1", "-t", "-A", "-c", statement}
	if declared, ok := e.session.manifest.Service(service); ok {
		// A manifest can name a different database client or role, e.g. for a
		// MySQL task, without touching the core.
		if cli := declared.Env["SQL_CLIENT"]; cli != "" {
			command = strings.Split(cli, " ")
			command = append(command, "-v", "ON_ERROR_STOP=1", "-t", "-A", "-c", statement)
		}
	}
	return e.adapter.Exec(ctx, e.session.ID, runtime.ExecRequest{Service: service, Command: command})
}

// verifyEnv is the verifier.Environment of one session. It is deliberately a
// different type from sessionEnv: an injector may write, a verifier may only
// read and run checks.
type verifyEnv struct {
	session *Session
	adapter runtime.Adapter
}

func (e *verifyEnv) InstanceID() string { return e.session.ID }

func (e *verifyEnv) Instance() *runtime.Instance { return e.session.instance }

func (e *verifyEnv) Runtime() runtime.Adapter { return e.adapter }

// Env returns the effective environment of a service, read through the engine.
func (e *verifyEnv) Env(ctx context.Context, service string) (map[string]string, error) {
	out, err := e.adapter.Exec(ctx, e.session.ID, runtime.ExecRequest{
		Service: service,
		Command: []string{"env"},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("read env of %s: %s", service, strings.TrimSpace(out.Stderr))
	}
	return parseEnv(out.Stdout), nil
}

// SQL runs a statement and returns its rows as string maps.
func (e *verifyEnv) SQL(ctx context.Context, service, statement string) ([]map[string]string, error) {
	// The statement is executed as the database role, not as root: the
	// official postgres image refuses a root connection, and a check that
	// cannot connect would fail for the wrong reason.
	command := []string{"gosu", "postgres", "psql", "-v", "ON_ERROR_STOP=1", "-t", "-A", "-F", "|", "-c", statement}
	if declared, ok := e.session.manifest.Service(service); ok {
		if cli := declared.Env["SQL_CLIENT"]; cli != "" {
			command = append(strings.Split(cli, " "), "-v", "ON_ERROR_STOP=1", "-t", "-A", "-F", "|", "-c", statement)
		}
	}
	res, err := e.adapter.Exec(ctx, e.session.ID, runtime.ExecRequest{
		Service: service,
		Command: command,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("sql on %s failed: %s", service, strings.TrimSpace(res.Stderr))
	}
	var rows []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		row := map[string]string{}
		for i, field := range fields {
			row["column_"+strconv.Itoa(i+1)] = field
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// parseEnv turns `env` output into a map.
func parseEnv(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			continue
		}
		out[key] = value
	}
	return out
}
