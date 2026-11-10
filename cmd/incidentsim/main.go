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
		return client(args[1:]).ExecSQL(context.Background(), requiredArg(args[1:], "session id"),
			flagValue(args[1:], "--service", ""), requiredFlagValue(args[1:], "--sql", "a --sql statement"))
	case "metrics":
		return client(args[1:]).Metrics(context.Background(), requiredArg(args[1:], "session id"), flagValue(args[1:], "--service", ""))
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
  incidentsim logs <session> [--service app] [-grep текст] [--tail N]
  incidentsim hint <session> [--level N]            подсказка по задаче
  incidentsim apply <session> --service app --set KEY=VALUE [--no-restart]
  incidentsim verify <session>                      correctness + quality gate
  incidentsim reset <session>                       восстановить baseline
  incidentsim stop <session>                        погасить окружение
  incidentsim sessions                              активные сессии
  incidentsim validate [dir]                        проверить манифесты каталога
  incidentsim demo [task]                           один сценарий от start до postmortem
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
	if len(sets) == 0 {
		return fmt.Errorf("at least one --set KEY=VALUE is required")
	}
	patch, err := core.ParseEnvPatch(service, sets, !hasFlag(args, "--no-restart"))
	if err != nil {
		return err
	}
	return client(args).Apply(context.Background(), sessionID, service, patch.Vars, patch.Restart)
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

// requiredFlagValue returns a flag value or stops with an explanation.
func requiredFlagValue(args []string, name, what string) string {
	if value := flagValue(args, name, ""); value != "" {
		return value
	}
	fmt.Fprintf(os.Stderr, "incidentsim: %s is required\n", what)
	os.Exit(2)
	return ""
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
