// Command incidentsim is the CLI of the IncidentSim core. It talks to the
// local HTTP API of the core; `incidentsim serve` starts that API.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"incidentsim/internal/adapters/compose"
	"incidentsim/internal/adapters/firecracker"
	"incidentsim/internal/adapters/teleport"
	"incidentsim/internal/cli"
	"incidentsim/internal/core"
	"incidentsim/internal/injector"
	"incidentsim/internal/runtime"
	"incidentsim/internal/verifier"
	"incidentsim/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "incidentsim: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "show":
		return client(args[1:]).Show(context.Background(), optionalArg(args[1:]))
	case "start":
		_, err := client(args[1:]).Start(context.Background(), requiredArg(args[1:], "task id"))
		return err
	case "exec":
		return runExec(args[1:])
	case "metrics":
		return client(args[1:]).Metrics(context.Background(), requiredArg(args[1:], "session id"), flagValue(args[1:], "--service", ""))
	case "env":
		return client(args[1:]).Env(context.Background(), requiredArg(args[1:], "session id"), flagValue(args[1:], "--service", ""))
	case "logs":
		return client(args[1:]).Logs(context.Background(), requiredArg(args[1:], "session id"), flagValue(args[1:], "--service", ""), flagValue(args[1:], "--grep", ""), intValue(flagValue(args[1:], "--tail", "200"), 200))
	case "hint":
		level := intValue(flagValue(args[1:], "--level", "0"), 0)
		return client(args[1:]).Hint(context.Background(), requiredArg(args[1:], "session id"), level)
	case "apply":
		return apply(args[1:])
	case "verify":
		return client(args[1:]).Verify(context.Background(), requiredArg(args[1:], "session id"))
	case "reset":
		return client(args[1:]).Reset(context.Background(), requiredArg(args[1:], "session id"))
	case "postmortem":
		return client(args[1:]).Postmortem(context.Background(), requiredArg(args[1:], "session id"), hasFlag(args[1:], "--md"))
	case "selfcheck":
		return selfcheck(args[1:])
	case "stop":
		return client(args[1:]).Stop(context.Background(), requiredArg(args[1:], "session id"))
	case "sessions":
		return client(args[1:]).Sessions(context.Background())
	case "validate":
		return validate(args[1:])
	case "demo":
		return demo(args[1:])
	case "version":
		fmt.Println(version.String())
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Print(`incidentsim — тренажёр разбора инцидентов

Использование:
  incidentsim serve [--config incidentsim.yaml]     запустить ядро (HTTP API)
  incidentsim show [task]                           список задач или одна задача
  incidentsim start <task>                          поднять окружение и активировать сбой
  incidentsim metrics <session> [--service app]     последние метрики сервиса
  incidentsim env <session> [--service app]          окружение сервиса (диагностика)
  incidentsim logs <session> [--service app] [-grep текст] [--tail N]
  incidentsim exec <session> --service app -- sh -c 'cat /etc/app/app.conf'
  incidentsim exec <session> --service db --sql "SELECT 1"
  incidentsim hint <session> [--level N]            подсказка по задаче
  incidentsim apply <session> --service app --set KEY=VALUE [--no-restart]
  incidentsim verify <session>                      correctness + quality gate
  incidentsim reset <session>                       восстановить baseline
  incidentsim stop <session>                        погасить окружение
  incidentsim sessions                              активные сессии
  incidentsim validate [dir]                        проверить манифесты каталога
  incidentsim demo [task]                           один сценарий от start до postmortem
  incidentsim selfcheck <task>                      прогнать reference и дистракторы
  incidentsim version

Переменные окружения:
  INCIDENTSIM_ADDR     адрес ядра (по умолчанию 127.0.0.1:8747)
  INCIDENTSIM_CONFIG   путь к incidentsim.yaml
`)
}

func client(args []string) *cli.Client {
	base := os.Getenv("INCIDENTSIM_ADDR")
	if base == "" {
		base = "http://127.0.0.1:8747"
	}
	if addr := flagValue(args, "--addr", ""); addr != "" {
		base = "http://" + addr
	}
	return cli.New(base)
}

func serve(args []string) error {
	configPath := flagValue(args, "--config", os.Getenv("INCIDENTSIM_CONFIG"))
	config, err := core.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if addr := flagValue(args, "--addr", ""); addr != "" {
		config.Listen = addr
	}

	catalog, err := core.NewCatalog(config.Catalog)
	if err != nil {
		return err
	}

	adapters := map[string]runtime.Adapter{}
	if config.Engine == "compose" || config.Engine == "" {
		adapters["compose"] = compose.New(config.StateDir())
	}
	if config.Engine == "firecracker" {
		adapters["firecracker"] = firecracker.New(firecracker.Config{
			FirecrackerBin: config.Firecracker.FirecrackerBin,
			SocketDir:      config.Firecracker.SocketDir,
			KernelImage:    config.Firecracker.KernelImage,
			RootfsDir:      filepath.Dir(config.Firecracker.RootfsImage),
			BaselineDir:    filepath.Dir(config.Firecracker.BaselineSnapshot),
			GuestAgentPort: config.Firecracker.GuestAgentPort,
		})
		// Compose stays registered: a manifest can still choose it, and the
		// core can fall back to it when KVM is unavailable on this host.
		adapters["compose"] = compose.New(config.StateDir())
	}

	grader := verifier.NewManifestVerifier()
	grader.ProjectDir = config.ProjectDir()
	orchestrator, err := core.New(config, catalog, adapters, injector.DefaultRegistry(), grader)
	if err != nil {
		return err
	}
	orchestrator.Builder = &core.GoBuilder{
		Image:      "golang:1.24-alpine",
		BuildCache: "incidentsim-go-build-cache",
	}
	// Session audit is optional: a missing Teleport degrades to a stub and a
	// broken one is a warning in the journal.
	orchestrator.Audit = teleport.FromConfig(teleport.Config{
		Enabled:  config.Teleport.Enabled,
		Endpoint: config.Teleport.Endpoint,
		Timeout:  30 * time.Second,
	})
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              config.Listen,
		Handler:           core.NewServer(orchestrator).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("incidentsim core listening on %s (engine %s, %d tasks)\n", config.Listen, config.Engine, len(catalog.IDs()))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

// apply parses the fix flags and pushes them into a session.
//
// A fix is either a configuration change (--set) or a statement (--sql): those
// are the two write paths a participant has, and both go through the same
// journal so the postmortem sees them the same way.
func apply(args []string) error {
	sessionID := requiredArg(args, "session id")
	service := flagValue(args, "--service", "")
	if statement := flagValue(args, "--sql", ""); statement != "" {
		return client(args).ExecSQL(context.Background(), sessionID, service, statement)
	}
	if service == "" {
		return fmt.Errorf("--service is required")
	}
	sets := multiFlag(args, "--set")
	files := multiFileFlag(args, "--file")
	contents := multiFlag(args, "--content")
	fileFromRef := multiFlag(args, "--file-from-reference")
	if len(fileFromRef) > 0 && os.Getenv("INCIDENTSIM_DEMO") != "1" {
		return fmt.Errorf("--file-from-reference is only available with INCIDENTSIM_DEMO=1")
	}
	if len(files) != len(contents) {
		return fmt.Errorf("each --file must have a matching --content")
	}
	if len(sets) == 0 && len(files) == 0 && len(fileFromRef) == 0 {
		return fmt.Errorf("at least one --set KEY=VALUE, --file PATH --content TEXT, or --file-from-reference PATH is required")
	}
	fileMap := map[string]string{}
	for i, f := range files {
		fileMap[f] = contents[i]
	}
	projectRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	for _, refPath := range fileFromRef {
		referenceDir, err := taskReferenceDir(projectRoot, client(args), sessionID)
		if err != nil {
			return err
		}
		content, err := readReferenceFile(referenceDir, refPath)
		if err != nil {
			return err
		}
		fileMap[refPath] = content
	}
	patch, err := core.ParseEnvPatch(service, sets, !hasFlag(args, "--no-restart"))
	if err != nil {
		return err
	}
	patch.Files = fileMap
	return client(args).Apply(context.Background(), sessionID, service, patch.Vars, patch.Files, patch.Restart)
}

// taskReferenceDir resolves the reference directory of a session's task.
//
// The reference path is read from the manifest (spec.reference) through the
// core API, so there is exactly one declaration of where a task's answer key
// lives. The manifest path is repository-relative, and the CLI runs from the
// repository root, so it is joined with the current directory.
//
// The session id encodes the task id as <task>-<seq>-<HHMMSS>, which is the
// only way a CLI invocation knows which task it is talking about.
func taskReferenceDir(projectRoot string, api *cli.Client, sessionID string) (string, error) {
	taskID, err := taskIDFromSession(sessionID)
	if err != nil {
		return "", err
	}
	var task struct {
		Reference string `json:"reference,omitempty"`
		Workspace string `json:"workspace,omitempty"`
	}
	if err := api.Get(context.Background(), "/api/v1/tasks/"+taskID, &task); err != nil {
		return "", fmt.Errorf("read task %q: %w", taskID, err)
	}
	if task.Reference != "" {
		return filepath.Join(projectRoot, task.Reference), nil
	}
	return filepath.Join(projectRoot, "scenarios", taskID, "reference"), nil
}

// taskIDFromSession recovers the task id from a session id, which the core
// builds as <taskID>-<sequence>-<HHMMSS>. A task id is itself kebab-case, so
// only the last two dash-separated segments belong to the core.
func taskIDFromSession(sessionID string) (string, error) {
	parts := strings.Split(sessionID, "-")
	if len(parts) < 3 {
		return "", fmt.Errorf("cannot determine task id from session %q", sessionID)
	}
	return strings.Join(parts[:len(parts)-2], "-"), nil
}

// runExec runs a command inside a service of a session.
//
// Two forms, one endpoint:
//
//	incidentsim exec <session> --service app -- sh -c 'cat /etc/app/app.conf'
//	incidentsim exec <session> --service db --sql "SELECT 1"
//
// The first is the shell into the environment a participant needs to debug an
// incident: cat, ps, ss, top, curl, a manual psql session, tcpdump. The second
// is sugar the core expands with the task's own database client.
func runExec(args []string) error {
	sessionID := requiredArg(args, "session id")
	service := flagValue(args, "--service", "")
	if statement := flagValue(args, "--sql", ""); statement != "" {
		return client(args).ExecSQL(context.Background(), sessionID, service, statement)
	}
	command := commandAfterSeparator(args)
	if len(command) == 0 {
		return fmt.Errorf("a command is required: incidentsim exec <session> --service app -- sh -c '...'")
	}
	if service == "" {
		return fmt.Errorf("--service is required")
	}
	return client(args).Exec(context.Background(), sessionID, service, command)
}

// commandAfterSeparator returns everything after a bare `--`, which is how a
// shell command is spelled on the command line without the flags eating it.
func commandAfterSeparator(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return args[i+1:]
		}
	}
	return nil
}

// readReferenceFile reads a file from the reference directory of a task.
func readReferenceFile(referenceDir, refPath string) (string, error) {
	// Reject absolute paths and ..-segments before joining:
	// a participant must not be able to read outside the reference directory.
	if filepath.IsAbs(refPath) {
		return "", fmt.Errorf("reference path %q must be relative", refPath)
	}
	for _, part := range strings.Split(refPath, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("reference path %q escapes the reference directory", refPath)
		}
	}

	path := filepath.Join(referenceDir, refPath)
	rel, err := filepath.Rel(referenceDir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("reference path %q escapes the reference directory", refPath)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read reference file %s: %w", path, err)
	}
	return string(data), nil
}

// validate checks a catalog directory without starting the core.
func validate(args []string) error {
	dir := optionalArg(args)
	if dir == "" {
		dir = "manifests"
	}
	catalog, err := core.NewCatalog(dir)
	if err != nil {
		return err
	}
	for _, task := range catalog.List() {
		fmt.Printf("ok %-28s %s (%d correctness, %d quality checks)\n", task.ID, task.Title, task.Correctness, task.Quality)
	}
	return nil
}

// demo runs one task end to end and prints the verification report. It is the
// fastest way to see the simulator work, and the script the README refers to.
func demo(args []string) error {
	taskID := optionalArg(args)
	if taskID == "" {
		taskID = "db-connection-leak"
	}
	api := client(args)

	var tasks struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if err := api.Show(context.Background(), ""); err != nil {
		return err
	}
	if err := api.Get(context.Background(), "/api/v1/tasks", &tasks); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, task := range tasks.Tasks {
		known[task.ID] = true
	}
	if !known[taskID] {
		return fmt.Errorf("task %q is not in the catalog", taskID)
	}

	fmt.Printf("\n=== start %s ===\n", taskID)
	sessionID, err := api.Start(context.Background(), taskID)
	if err != nil {
		return err
	}

	fmt.Printf("\n=== hint ===\n")
	if err := api.Hint(context.Background(), sessionID, 0); err != nil {
		return err
	}

	fmt.Printf("\n=== verify before any fix ===\n")
	if err := api.Verify(context.Background(), sessionID); err != nil {
		fmt.Println("ожидаемо: окружение сломано, проверка не проходит")
	}

	fmt.Printf("\n=== metrics ===\n")
	if err := api.Metrics(context.Background(), sessionID, ""); err != nil {
		fmt.Println("метрики недоступны:", err)
	}

	if err := api.Reset(context.Background(), sessionID); err != nil {
		fmt.Println("reset:", err)
	}
	if err := api.Stop(context.Background(), sessionID); err != nil {
		fmt.Println("stop:", err)
	}
	return nil
}

// selfcheck runs a task end to end: start, apply the reference fix,
// verify (expect pass), reset, then apply each distractor and verify
// (expect partial_fix). It exits 0 only when every step passes.
func selfcheck(args []string) error {
	taskID := requiredArg(args, "task id")
	api := client(args)

	// 1. Task must exist in the catalog.
	var tasks struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if err := api.Get(context.Background(), "/api/v1/tasks", &tasks); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, t := range tasks.Tasks {
		known[t.ID] = true
	}
	if !known[taskID] {
		return fmt.Errorf("task %q is not in the catalog", taskID)
	}

	// 2. The task detail carries everything the run needs: the level, the
	// editable files, the reference directory the manifest declares and the
	// distractors. Reading them from one place keeps the CLI from keeping a
	// second, drifting copy of the task definition.
	task, err := readTaskDetail(api, taskID)
	if err != nil {
		return err
	}
	fmt.Printf("=== selfcheck %s (level=%s) ===\n", taskID, task.Level)

	// A run without distractors cannot prove that a false fix is caught.
	if len(task.Distractors) == 0 {
		return fmt.Errorf("task %q declares no distractors in the API response: selfcheck cannot prove a false fix is caught", taskID)
	}

	// 3. Start the task.
	sessionID, err := api.Start(context.Background(), taskID)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer func() {
		_ = api.Stop(context.Background(), sessionID)
	}()
	fmt.Printf("start: PASS (session=%s)\n", sessionID)

	// 4. Apply the reference fix based on level.
	projectRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := applyReferenceFix(api, sessionID, task, referenceDirFor(projectRoot, task)); err != nil {
		return fmt.Errorf("apply reference fix: %w", err)
	}
	fmt.Printf("apply reference: PASS\n")

	// 5. Verify after reference fix: must pass.
	if err := verifyExpect(api, sessionID, verifier.VerdictPass); err != nil {
		return fmt.Errorf("verify after reference: %w", err)
	}
	fmt.Printf("verify reference: PASS\n")

	// 6. Reset back to the broken state.
	if err := api.Reset(context.Background(), sessionID); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	fmt.Printf("reset: PASS\n")

	// 7. For each distractor: apply, verify (expect partial_fix), reset.
	for _, d := range task.Distractors {
		fmt.Printf("  distractor %s: %s\n", d.ID, d.Title)
		if err := applyDistractor(api, context.Background(), sessionID, d, task.Level); err != nil {
			return fmt.Errorf("apply distractor %s: %w", d.ID, err)
		}
		if err := verifyExpect(api, sessionID, verifier.VerdictPartial); err != nil {
			return fmt.Errorf("verify distractor %s: %w", d.ID, err)
		}
		if err := api.Reset(context.Background(), sessionID); err != nil {
			return fmt.Errorf("reset after distractor %s: %w", d.ID, err)
		}
		fmt.Printf("  distractor %s: PASS (partial_fix)\n", d.ID)
	}

	fmt.Printf("=== selfcheck %s: ALL PASS ===\n", taskID)
	return nil
}

// taskDetail is the catalog view of one task the selfcheck needs.
type taskDetail struct {
	ID          string   `json:"id"`
	Level       string   `json:"level"`
	Editable    []string `json:"editable,omitempty"`
	Reference   string   `json:"reference,omitempty"`
	Distractors []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Fails string `json:"fails"`
	} `json:"distractors,omitempty"`
}

// referenceDirFor resolves the reference directory of a task from the
// manifest declaration (spec.reference), falling back to
// scenarios/<task>/reference for a manifest that does not declare one.
func referenceDirFor(projectRoot string, task taskDetail) string {
	if task.Reference != "" {
		return filepath.Join(projectRoot, task.Reference)
	}
	return filepath.Join(projectRoot, "scenarios", task.ID, "reference")
}

// readTaskDetail reads the catalog detail of a task.
func readTaskDetail(api *cli.Client, taskID string) (taskDetail, error) {
	var task taskDetail
	if err := api.Get(context.Background(), "/api/v1/tasks/"+taskID, &task); err != nil {
		return task, fmt.Errorf("read task %q: %w", taskID, err)
	}
	if task.Level == "" {
		return task, fmt.Errorf("task %q declares no level", taskID)
	}
	return task, nil
}

// splitSQLStatements splits a reference .sql file into individual statements.
// Comments and empty lines are dropped; a trailing semicolon is trimmed.
func splitSQLStatements(content string) []string {
	var out []string
	for _, chunk := range strings.Split(content, ";") {
		var lines []string
		for _, line := range strings.Split(chunk, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				continue
			}
			lines = append(lines, line)
		}
		stmt := strings.TrimSpace(strings.Join(lines, "\n"))
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// applyReferenceFix applies the reference solution based on task level.
func applyReferenceFix(api *cli.Client, sessionID string, task taskDetail, referenceDir string) error {
	service := "app"
	switch task.Level {
	case "code":
		// For code-level tasks, apply the reference source file.
		if len(task.Editable) == 0 {
			return fmt.Errorf("code-level task %q has no editable files", task.ID)
		}
		refPath := task.Editable[0]
		content, err := readReferenceFile(referenceDir, refPath)
		if err != nil {
			return fmt.Errorf("read reference %s: %w", refPath, err)
		}
		return api.Apply(context.Background(), sessionID, service, nil,
			map[string]string{refPath: content}, true)
	case "config":
		// For config-level tasks, apply the reference config file.
		if len(task.Editable) == 0 {
			return fmt.Errorf("config-level task %q has no editable files", task.ID)
		}
		refPath := task.Editable[0]
		content, err := readReferenceFile(referenceDir, refPath)
		if err != nil {
			return fmt.Errorf("read reference %s: %w", refPath, err)
		}
		return api.Apply(context.Background(), sessionID, service, nil,
			map[string]string{refPath: content}, true)
	case "db":
		// For db-level tasks, apply each statement in reference solution.sql.
		content, err := readReferenceFile(referenceDir, "solution.sql")
		if err != nil {
			return fmt.Errorf("read reference solution.sql: %w", err)
		}
		for _, stmt := range splitSQLStatements(content) {
			if err := api.ExecSQL(context.Background(), sessionID, "db", stmt); err != nil {
				return fmt.Errorf("apply reference statement %q: %w", stmt, err)
			}
		}
		return nil
	case "composite":
		// For composite tasks, apply all reference config files.
		if len(task.Editable) == 0 {
			return fmt.Errorf("composite-level task %q has no editable files", task.ID)
		}
		for _, refPath := range task.Editable {
			content, err := readReferenceFile(referenceDir, refPath)
			if err != nil {
				return fmt.Errorf("read reference %s: %w", refPath, err)
			}
			if err := api.Apply(context.Background(), sessionID, service, nil,
				map[string]string{refPath: content}, true); err != nil {
				return err
			}
		}
		// A composite fix is config + SQL: run solution.sql if present.
		if content, err := readReferenceFile(referenceDir, "solution.sql"); err == nil {
			for _, stmt := range splitSQLStatements(content) {
				if err := api.ExecSQL(context.Background(), sessionID, "db", stmt); err != nil {
					return fmt.Errorf("apply reference statement %q: %w", stmt, err)
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown level %q", task.Level)
	}
}

// verifyExpect calls verify and checks that the verdict matches want.
func verifyExpect(api *cli.Client, sessionID, want string) error {
	var report verifier.Report
	if err := api.Post(context.Background(), "/api/v1/sessions/"+sessionID+"/verify", nil, &report); err != nil {
		return fmt.Errorf("verify request: %w", err)
	}
	if report.Verdict != want {
		return fmt.Errorf("verdict = %s, want %s\n%s", report.Verdict, want, report.String())
	}
	return nil
}

// applyDistractor applies a distractor fix based on its fails check.
//
// The switch is the mapping from "what this distractor hides" to "the action
// that hides it". Every distractor of the catalog must have its own branch
// with its own fails check: two distractors sharing a fails value are a task
// bug, because they would apply the same action and grade the same way.
func applyDistractor(api *cli.Client, ctx context.Context, sessionID string, d struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Fails string `json:"fails"`
}, level string) error {
	service := "app"
	switch d.Fails {
	case "pool-in-use-not-maxed":
		// bigger-pool: set POOL_MAX higher
		return api.Apply(context.Background(), sessionID, service,
			map[string]string{"POOL_MAX": "100"}, nil, true)
	case "idle-in-transaction-bounded":
		// restart-service: restart the service. The connections are freed,
		// so the system answers again, but the leaking code is still there
		// and the leak re-forms under the replayed load.
		return api.Apply(ctx, sessionID, service,
			map[string]string{}, nil, true)
	case "retry-loop-bounded":
		// disable-retry: set RETRY_ENABLED=false
		return api.Apply(context.Background(), sessionID, service,
			map[string]string{"RETRY_ENABLED": "false"}, nil, true)
	case "cache-back-on":
		// restart-app: just restart (env already broken from start)
		return api.Apply(context.Background(), sessionID, service,
			map[string]string{}, nil, true)
	case "log-history-preserved":
		// delete-log-file: LOG_FILE=/dev/null makes the preserved-log check fail
		// (empty file) while freeing the disk; behaviourally a deleted log.
		return api.Apply(ctx, sessionID, "app", nil,
			map[string]string{"config/app.conf": "LOG_LEVEL=debug\nLOG_FILE=/dev/null\n"}, true)
	case "data-preserved":
		// truncate-orders: fast queries, no data.
		return api.ExecSQL(ctx, sessionID, "db", "TRUNCATE orders")
	case "autovacuum-enabled":
		// Keep autovacuum off; the symptom is not what the check catches.
		return api.ExecSQL(ctx, sessionID, "db", "ALTER TABLE orders SET (autovacuum_enabled = false)")
	case "connection-limit-restored":
		// Squeeze the connection limit instead of fixing the cache/retry loop.
		return api.ExecSQL(ctx, sessionID, "db", "ALTER ROLE postgres CONNECTION LIMIT 10")
	default:
		return fmt.Errorf("unknown distractor fails check %q", d.Fails)
	}
}

// --- flag helpers -----------------------------------------------------------

func optionalArg(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

func requiredArg(args []string, what string) string {
	if value := optionalArg(args); value != "" {
		return value
	}
	fmt.Fprintf(os.Stderr, "incidentsim: %s is required\n", what)
	os.Exit(2)
	return ""
}

func flagValue(args []string, name, fallback string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"=")
		}
	}
	return fallback
}

func multiFlag(args []string, name string) []string {
	var out []string
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			out = append(out, args[i+1])
		}
		if strings.HasPrefix(arg, name+"=") {
			out = append(out, strings.TrimPrefix(arg, name+"="))
		}
	}
	return out
}

func multiFileFlag(args []string, name string) []string {
	var out []string
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			out = append(out, args[i+1])
		}
		if strings.HasPrefix(arg, name+"=") {
			out = append(out, strings.TrimPrefix(arg, name+"="))
		}
	}
	return out
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func intValue(text string, fallback int) int {
	parsed, err := strconv.Atoi(text)
	if err != nil {
		return fallback
	}
	return parsed
}
