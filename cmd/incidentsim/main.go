// Command incidentsim is the CLI of the IncidentSim core. It talks to the
// local HTTP API of the core; `incidentsim serve` starts that API.
//
// Every subcommand parses its own flag.FlagSet, so a positional session/task
// id is never confused with a flag value (the optionalArg bug). A session id
// may be given positionally or with --session.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
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
		return showCmd(args[1:])
	case "start":
		return startCmd(args[1:])
	case "status":
		return statusCmd(args[1:])
	case "logs":
		return logsCmd(args[1:])
	case "metrics":
		return metricsCmd(args[1:])
	case "env":
		return envCmd(args[1:])
	case "exec":
		return execCmd(args[1:])
	case "sql":
		return sqlCmd(args[1:])
	case "shell":
		return shellCmd(args[1:])
	case "edit":
		return editCmd(args[1:])
	case "diff":
		return diffCmd(args[1:])
	case "apply":
		return applyCmd(args[1:])
	case "verify":
		return verifyCmd(args[1:])
	case "hint":
		return hintCmd(args[1:])
	case "reset":
		return resetCmd(args[1:])
	case "reveal":
		return revealCmd(args[1:])
	case "give-up":
		return giveUpCmd(args[1:])
	case "postmortem":
		return postmortemCmd(args[1:])
	case "stop":
		return stopCmd(args[1:])
	case "sessions":
		return client(nil).Sessions(context.Background())
	case "validate":
		return validateCmd(args[1:])
	case "selfcheck":
		return selfcheckCmd(args[1:])
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
  incidentsim serve [--config incidentsim.yaml] [--addr host:port]   запустить ядро (HTTP API)
  incidentsim show [task]                                          список задач или одна задача (только публичное)
  incidentsim start <task>                                         поднять окружение и активировать сбой
  incidentsim status <session>                                     состояние сессии
  incidentsim logs <session> [--service app] [--grep текст] [--tail N]  логи сервиса
  incidentsim metrics <session> [--service app]                    метрики сервиса
  incidentsim env <session> [--service app]                        окружение сервиса (диагностика)
  incidentsim exec <session> --service app -- sh -c 'cat ...'      команда в сервисе
  incidentsim sql <session> --service db "SELECT 1"                SQL-запрос к БД
  incidentsim shell <session> [--service app]                      интерактивный терминал
  incidentsim edit <session> [файл]                                открыть $EDITOR на файле workspace
  incidentsim diff <session>                                       правки относительно исходного workspace
  incidentsim apply <session> [--service app] [--file P --content T] применить фикс из workspace
  incidentsim verify <session>                                     correctness + quality gate
  incidentsim hint <session> [--level N]                           подсказка по задаче
  incidentsim reset <session>                                      восстановить baseline
  incidentsim reveal <session>                                     ответ и разбор (после pass или give-up)
  incidentsim give-up <session>                                    сдаться: открытьprivate вид задачи
  incidentsim postmortem <session> [--md]                          постмортем сессии
  incidentsim stop <session>                                       погасить окружение
  incidentsim sessions                                             активные сессии
  incidentsim validate [dir]                                       проверить манифесты каталога
  incidentsim selfcheck <task>                                     прогнать reference и дистракторы
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
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", os.Getenv("INCIDENTSIM_CONFIG"), "путь к incidentsim.yaml")
	addr := fs.String("addr", "", "адрес сервера (переопределяет конфиг)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	config, err := core.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *addr != "" {
		config.Listen = *addr
	}

	catalog, err := core.NewCatalog(config.CatalogDir())
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
	orchestrator.Audit = teleport.FromConfig(teleport.Config{
		Enabled:  config.Teleport.Enabled,
		Endpoint: config.Teleport.Endpoint,
		Timeout:  30 * time.Second,
	})

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

// sessionID resolves the session id: the --session flag value wins if set,
// otherwise the first positional argument. The flag default "__unset__" is a
// sentinel that is never a real session id.
func sessionID(sessionFlag string, positional []string) (string, error) {
	if sessionFlag != "" && sessionFlag != "__unset__" {
		return sessionFlag, nil
	}
	if len(positional) > 0 {
		return positional[0], nil
	}
	return "", fmt.Errorf("session id is required (positional or --session)")
}

func showCmd(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := client(nil)
	if fs.NArg() == 0 {
		return c.Show(context.Background(), "")
	}
	return c.Show(context.Background(), fs.Arg(0))
}

func startCmd(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	task, err := taskIDArg(fs.Args())
	if err != nil {
		return err
	}
	sessionID, err := client(nil).Start(context.Background(), task)
	if err != nil {
		return err
	}
	fmt.Printf("session %s started\n", sessionID)
	return nil
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	c := client(nil)
	var view struct {
		Session struct {
			ID      string `json:"id"`
			State   string `json:"state"`
			TaskID  string `json:"task_id"`
			Verdict string `json:"last_verdict"`
			Engine  string `json:"engine"`
		} `json:"session"`
	}
	if err := c.Get(context.Background(), "/api/v1/sessions/"+id, &view); err != nil {
		return err
	}
	s := view.Session
	fmt.Printf("session %s (%s, %s, engine %s)\n", s.ID, s.TaskID, s.State, s.Engine)
	if s.Verdict != "" {
		fmt.Printf("verdict: %s\n", s.Verdict)
	}
	return nil
}

func logsCmd(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	service := fs.String("service", "", "сервис")
	grep := fs.String("grep", "", "фильтр строк")
	tail := fs.Int("tail", 200, "количество строк")
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	return client(nil).Logs(context.Background(), id, *service, *grep, *tail)
}

func metricsCmd(args []string) error {
	fs := flag.NewFlagSet("metrics", flag.ExitOnError)
	service := fs.String("service", "", "сервис")
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	return client(nil).Metrics(context.Background(), id, *service)
}

func envCmd(args []string) error {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	service := fs.String("service", "", "сервис")
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	return client(nil).Env(context.Background(), id, *service)
}

func execCmd(args []string) error {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	service := fs.String("service", "", "сервис")
	tty := fs.Bool("tty", false, "выделить терминал")
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	c := client(nil)
	statement := flagValue(args, "--sql", "")
	if statement != "" {
		return c.ExecSQL(context.Background(), id, *service, statement)
	}
	command := commandAfterSeparator(fs.Args())
	if len(command) == 0 {
		return fmt.Errorf("a command is required: incidentsim exec <session> --service app -- sh -c '...'")
	}
	if *service == "" {
		return fmt.Errorf("--service is required")
	}
	if *tty {
		return c.Shell(context.Background(), id, *service)
	}
	result, err := execClient(c, context.Background(), id, *service, command, "", 0)
	if err != nil {
		return err
	}
	return ReportExecResult(result)
}

func sqlCmd(args []string) error {
	fs := flag.NewFlagSet("sql", flag.ExitOnError)
	service := fs.String("service", "db", "сервис БД")
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	statement := strings.Join(fs.Args(), " ")
	if statement == "" {
		return fmt.Errorf("a statement is required: incidentsim sql <session> --service db 'SELECT 1'")
	}
	return client(nil).ExecSQL(context.Background(), id, *service, statement)
}

func shellCmd(args []string) error {
	fs := flag.NewFlagSet("shell", flag.ExitOnError)
	service := fs.String("service", "app", "сервис")
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	return client(nil).Shell(context.Background(), id, *service)
}

func editCmd(args []string) error {
	fs := flag.NewFlagSet("edit", flag.ExitOnError)
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	var file string
	if fs.NArg() > 1 {
		file = fs.Arg(1)
	}
	return client(nil).Edit(context.Background(), id, file)
}

func diffCmd(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	return client(nil).Diff(context.Background(), id)
}

func applyCmd(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	service := fs.String("service", "", "сервис")
	noRestart := fs.Bool("no-restart", false, "не перезапускать сервис после применения")
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	c := client(nil)
	if statement := flagValue(args, "--sql", ""); statement != "" {
		return c.ExecSQL(context.Background(), id, *service, statement)
	}
	if *service == "" {
		return fmt.Errorf("--service is required")
	}
	sets := multiFlag(args, "--set")
	if len(sets) > 0 {
		return fmt.Errorf("--set убран из пользовательского CLI: правите файл в workspace и запустите incidentsim apply <session>")
	}
	files := multiFileFlag(args, "--file")
	contents := multiFlag(args, "--content")
	fileFromRef := multiFlag(args, "--file-from-reference")
	if len(files) != len(contents) {
		return fmt.Errorf("each --file must have a matching --content")
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
		referenceDir, err := taskReferenceDir(projectRoot, c, id)
		if err != nil {
			return err
		}
		content, err := readReferenceFile(referenceDir, refPath)
		if err != nil {
			return err
		}
		fileMap[refPath] = content
	}
	patch, err := core.ParseEnvPatch(*service, sets, !*noRestart)
	if err != nil {
		return err
	}
	patch.Files = fileMap
	return c.Apply(context.Background(), id, *service, nil, patch.Files, patch.Restart)
}

func verifyCmd(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	return client(nil).Verify(context.Background(), id)
}

func hintCmd(args []string) error {
	fs := flag.NewFlagSet("hint", flag.ExitOnError)
	level := fs.Int("level", 0, "уровень подсказки")
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	return client(nil).Hint(context.Background(), id, *level)
}

func resetCmd(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	return client(nil).Reset(context.Background(), id)
}

func giveUpCmd(args []string) error {
	fs := flag.NewFlagSet("give-up", flag.ExitOnError)
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	return client(nil).GiveUp(context.Background(), id)
}

func revealCmd(args []string) error {
	fs := flag.NewFlagSet("reveal", flag.ExitOnError)
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	// reveal is gated to post-pass/give-up by the core (B4); for now it is an
	// alias to postmortem.
	return postmortemCmd(append([]string{"--md", "--session", id}, fs.Args()...))
}

func postmortemCmd(args []string) error {
	fs := flag.NewFlagSet("postmortem", flag.ExitOnError)
	markdown := fs.Bool("md", false, "вывод в формате markdown")
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	return client(nil).Postmortem(context.Background(), id, *markdown)
}

func stopCmd(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	sessionFlag := fs.String("session", "__unset__", "id сессии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := sessionID(*sessionFlag, fs.Args())
	if err != nil {
		return err
	}
	return client(nil).Stop(context.Background(), id)
}

func validateCmd(args []string) error {
	dir := "manifests"
	if len(args) > 0 {
		dir = args[0]
	}
	catalog, err := core.NewCatalog(dir)
	if err != nil {
		return err
	}
	for _, task := range catalog.List() {
		fmt.Printf("ok %-28s %s (%d correctness, %d quality checks)\n", task.ID, task.Title, task.Correctness, task.Quality)
	}
	// B4: workspace не должен содержать подсказок-спойлеров (BUG/BROKEN/FIXED
	// и им подобные). Проверка сканирует текст файлов workspace на запретные
	// слова, чтобы участник не нашёл ответ в заготовке.
	spoilerRe := regexp.MustCompile(`(?i)\b(bug|broken|fixme|leak)\b`)
	for _, task := range catalog.List() {
		workspace := filepath.Join(".", "scenarios", task.ID, "workspace")
		if err := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(data), "\n") {
				// Only comments count as spoilers: code may legitimately
				// mention "leak" in an identifier or a test name.
				trimmed := strings.TrimSpace(line)
				if !strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "/*") && !strings.HasPrefix(trimmed, "*") {
					continue
				}
				if m := spoilerRe.FindStringSubmatch(line); m != nil {
					return fmt.Errorf("spoiler word %q in comment %q (%s, task %s): workspace must not contain BUG/BROKEN/FIXED-style comments", m[1], trimmed, path, task.ID)
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func selfcheckCmd(args []string) error {
	fs := flag.NewFlagSet("selfcheck", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	task, err := taskIDArg(fs.Args())
	if err != nil {
		return err
	}
	return selfcheck(context.Background(), client(nil), task)
}

// --- helpers ---

// execClient posts either a command or a statement and returns the raw outcome.
func execClient(c *cli.Client, ctx context.Context, sessionID, service string, command []string, statement string, timeoutMS int) (cli.ExecResult, error) {
	body := map[string]any{"service": service}
	if len(command) > 0 {
		body["command"] = command
	}
	if statement != "" {
		body["statement"] = statement
	}
	if timeoutMS > 0 {
		body["timeout_ms"] = timeoutMS
	}
	var response struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := c.Post(ctx, "/api/v1/sessions/"+sessionID+"/exec", body, &response); err != nil {
		return cli.ExecResult{}, err
	}
	return cli.ExecResult{ExitCode: response.ExitCode, Stdout: response.Stdout, Stderr: response.Stderr}, nil
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

func commandAfterSeparator(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return args[i+1:]
		}
	}
	return nil
}

// taskReferenceDir resolves the reference directory of a session's task.
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

// taskIDArg returns the single positional task id argument.
func taskIDArg(positional []string) (string, error) {
	if len(positional) == 0 {
		return "", fmt.Errorf("task id is required")
	}
	return positional[0], nil
}

// taskIDFromSession recovers the task id from a session id, which the core
// builds as <taskID>-<sequence>-<HHMMSS>.
func taskIDFromSession(sessionID string) (string, error) {
	parts := strings.Split(sessionID, "-")
	if len(parts) < 3 {
		return "", fmt.Errorf("cannot determine task id from session %q", sessionID)
	}
	return strings.Join(parts[:len(parts)-2], "-"), nil
}

// readReferenceFile reads a file from the reference directory of a task.
func readReferenceFile(referenceDir, refPath string) (string, error) {
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

// splitSQLStatements splits a reference .sql file into individual statements.
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
		Apply struct {
			Kind    string            `json:"kind,omitempty"`
			Service string            `json:"service,omitempty"`
			Vars    map[string]string `json:"vars,omitempty"`
			From    string            `json:"from,omitempty"`
			To      string            `json:"to,omitempty"`
			SQL     []string          `json:"sql,omitempty"`
			Limits  map[string]string `json:"limits,omitempty"`
		} `json:"apply,omitempty"`
	} `json:"distractors,omitempty"`
}

// referenceDirFor resolves the reference directory of a task from the
// manifest declaration (spec.reference), falling back to
// scenarios/<task>/reference.
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
		return task, fmt.Errorf("task %q declares no level", task.ID)
	}
	return task, nil
}

// readAdminTaskDetail reads the full (private) task detail from the admin
// endpoint, including distractors, root_cause, theory and reference_solution.
// Requires INCIDENTSIM_ADMIN=1.
func readAdminTaskDetail(api *cli.Client, taskID string) (taskDetail, error) {
	var task taskDetail
	if err := api.Get(context.Background(), "/api/v1/admin/tasks/"+taskID, &task); err != nil {
		return task, fmt.Errorf("read admin task %q: %w", taskID, err)
	}
	if task.Level == "" {
		return task, fmt.Errorf("task %q declares no level", task.ID)
	}
	return task, nil
}

// selfcheck runs a task end to end: start, apply the reference fix,
// verify (expect pass), reset, then apply each distractor and verify
// (expect partial_fix). It exits 0 only when every step passes.
func selfcheck(ctx context.Context, api *cli.Client, taskID string) error {
	var tasks struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if err := api.Get(ctx, "/api/v1/tasks", &tasks); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, t := range tasks.Tasks {
		known[t.ID] = true
	}
	if !known[taskID] {
		return fmt.Errorf("task %q is not in the catalog", taskID)
	}

	// B4/B11: distractors are not in the public view. Read the full task
	// (including distractors and reference) from the admin endpoint when
	// INCIDENTSIM_ADMIN=1; otherwise selfcheck cannot test distractors.
	admin := os.Getenv("INCIDENTSIM_ADMIN") == "1"
	var task taskDetail
	var err error
	if admin {
		task, err = readAdminTaskDetail(api, taskID)
	} else {
		task, err = readTaskDetail(api, taskID)
	}
	if err != nil {
		return err
	}
	fmt.Printf("=== selfcheck %s (level=%s) ===\n", taskID, task.Level)

	if len(task.Distractors) == 0 {
		return fmt.Errorf("task %q declares no distractors: selfcheck cannot prove a false fix is caught", taskID)
	}

	sessionID, err := api.Start(ctx, taskID)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer func() {
		_ = api.Stop(ctx, sessionID)
	}()
	fmt.Printf("start: PASS (session=%s)\n", sessionID)

	projectRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := applyReferenceFix(api, sessionID, task, referenceDirFor(projectRoot, task)); err != nil {
		return fmt.Errorf("apply reference fix: %w", err)
	}
	fmt.Printf("apply reference: PASS\n")

	if err := verifyExpect(api, sessionID, verifier.VerdictPass); err != nil {
		return fmt.Errorf("verify after reference: %w", err)
	}
	fmt.Printf("verify reference: PASS\n")

	if err := api.Reset(ctx, sessionID); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	fmt.Printf("reset: PASS\n")

	for _, d := range task.Distractors {
		fmt.Printf("  distractor %s: %s\n", d.ID, d.Title)
		failsCheck, err := api.Distractor(ctx, sessionID, d.ID)
		if err != nil {
			return fmt.Errorf("apply distractor %s: %w", d.ID, err)
		}
		if failsCheck != d.Fails {
			return fmt.Errorf("distractor %s fails check %q, want %q", d.ID, failsCheck, d.Fails)
		}
		if err := verifyExpect(api, sessionID, verifier.VerdictPartial); err != nil {
			return fmt.Errorf("verify distractor %s: %w", d.ID, err)
		}
		if err := api.Reset(ctx, sessionID); err != nil {
			return fmt.Errorf("reset after distractor %s: %w", d.ID, err)
		}
		fmt.Printf("  distractor %s: PASS (partial_fix, fails %s)\n", d.ID, failsCheck)
	}

	fmt.Printf("=== selfcheck %s: ALL PASS ===\n", taskID)
	return nil
}

// applyReferenceFix applies the reference solution based on task level.
func applyReferenceFix(api *cli.Client, sessionID string, task taskDetail, referenceDir string) error {
	service := "app"
	switch task.Level {
	case "code":
		if len(task.Editable) == 0 {
			return fmt.Errorf("code-level task %q has no editable files", task.ID)
		}
		refPath := task.Editable[0]
		content, err := readReferenceFile(referenceDir, refPath)
		if err != nil {
			return fmt.Errorf("read reference %s: %w", refPath, err)
		}
		return api.Apply(context.Background(), sessionID, service, nil, map[string]string{refPath: content}, true)
	case "config":
		if len(task.Editable) == 0 {
			return fmt.Errorf("config-level task %q has no editable files", task.ID)
		}
		refPath := task.Editable[0]
		content, err := readReferenceFile(referenceDir, refPath)
		if err != nil {
			return fmt.Errorf("read reference %s: %w", refPath, err)
		}
		return api.Apply(context.Background(), sessionID, service, nil, map[string]string{refPath: content}, true)
	case "db":
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
		if len(task.Editable) == 0 {
			return fmt.Errorf("composite-level task %q has no editable files", task.ID)
		}
		for _, refPath := range task.Editable {
			content, err := readReferenceFile(referenceDir, refPath)
			if err != nil {
				return fmt.Errorf("read reference %s: %w", refPath, err)
			}
			if err := api.Apply(context.Background(), sessionID, service, nil, map[string]string{refPath: content}, true); err != nil {
				return err
			}
		}
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

// ReportExecResult prints what the command produced and turns a non-zero exit
// into an error.
func ReportExecResult(result cli.ExecResult) error {
	if result.Stdout != "" {
		fmt.Println(strings.TrimSpace(result.Stdout))
	}
	if result.Stderr != "" {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(result.Stderr))
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("command exited %d", result.ExitCode)
	}
	return nil
}
