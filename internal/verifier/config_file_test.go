package verifier

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"incidentsim/internal/runtime"
)

// configFileScenario is the state a config-level fix leaves behind: the
// participant edited the configuration file, the process environment still
// carries the value the service was started with. The ExecFunc models the
// compose adapter: the file is read with `cat` inside the container, so the
// check sees the file, not a bookkeeping copy.
func configFileScenario(t *testing.T, fileContent string) (FakeEnvironment, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	logPath := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logPath, []byte("2026-01-01 request served\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := FakeEnvironment{
		Services: []runtime.Service{{Name: "app", Endpoint: server.Listener.Addr().String()}},
		// The environment still says debug: only the file was fixed.
		EnvValues: map[string]map[string]string{
			"app": {"LOG_LEVEL": "debug", "LOG_FILE": logPath},
		},
		ExecFunc: func(req runtime.ExecRequest) (runtime.ExecResult, error) {
			if req.Command[0] == "cat" {
				return runtime.ExecResult{ExitCode: 0, Stdout: fileContent}, nil
			}
			return runtime.ExecResult{ExitCode: 1}, fmt.Errorf("unexpected command %v", req.Command)
		},
	}
	return env, logPath
}

func logLevelCheck(file string) Check {
	return Check{
		ID:      "log-level-info",
		Kind:    CheckPolicy,
		Service: "app",
		Rule:    RuleConfigFlagEquals,
		File:    file,
		Query:   "LOG_LEVEL",
		Expect:  &Expectation{Equals: strPtr("info")},
		Reason:  "debug logging is not acceptable in production",
	}
}

func healthCheck() Check {
	return Check{
		ID: "healthz-200", Kind: CheckHTTP, Service: "app", Path: "/healthz",
		Expect: &Expectation{Status: intPtr(200)},
	}
}

// TestFileBasedCheckSeesTheFixTheEnvCheckCannot is the invariant behind a
// config-level task: the fix lives in the configuration file, the process
// environment keeps the old value, and a check that reads the file must pass
// where a check that reads the environment fails. A grader that reads the
// environment would call a correct fix a failure.
func TestFileBasedCheckSeesTheFixTheEnvCheckCannot(t *testing.T) {
	env, _ := configFileScenario(t, "LOG_LEVEL=info\nLOG_FILE=/var/log/app/app.log\n")
	grader := NewManifestVerifier()

	report, err := grader.Verify(context.Background(), env, "session-1", "log-flood-disk", CheckSet{
		Correctness: []Check{healthCheck()},
		Quality:     []Check{logLevelCheck("/etc/app/app.conf")},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.Passed() {
		t.Fatalf("verdict = %s, want pass: the file is fixed\n%s", report.Verdict, report.String())
	}
	passed := report.Quality.Results[0]
	if passed.Evidence == "" || !strings.Contains(passed.Evidence, "/etc/app/app.conf") {
		t.Fatalf("evidence = %q, want it to name the file that was read", passed.Evidence)
	}

	// The same environment graded on the environment instead of the file is
	// still broken: that is the desync the file-based rule exists for.
	envCheck := logLevelCheck("/etc/app/app.conf")
	envCheck.Rule = RuleEnvFlagEquals
	envCheck.File = "" // no file declared: the check reads the process environment
	report, err = grader.Verify(context.Background(), env, "session-1", "log-flood-disk", CheckSet{
		Correctness: []Check{healthCheck()},
		Quality:     []Check{envCheck},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.Quality.Verdict != VerdictFail {
		t.Fatalf("quality = %s, want fail: the process environment still says debug", report.Quality.Verdict)
	}
	if report.Verdict != VerdictPartial {
		t.Fatalf("verdict = %s, want partial_fix", report.Verdict)
	}
}

// TestFileBasedCheckStillCatchesAnUnfixedConfiguration is the other half: a
// file that still says debug must fail the file-based rule, otherwise the rule
// grades nothing.
func TestFileBasedCheckStillCatchesAnUnfixedConfiguration(t *testing.T) {
	env, _ := configFileScenario(t, "LOG_LEVEL=debug\nLOG_FILE=/var/log/app/app.log\n")
	grader := NewManifestVerifier()

	report, err := grader.Verify(context.Background(), env, "session-1", "log-flood-disk", CheckSet{
		Correctness: []Check{healthCheck()},
		Quality:     []Check{logLevelCheck("/etc/app/app.conf")},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	failed := report.Quality.FailedResults()
	if len(failed) != 1 || failed[0].Check.ID != "log-level-info" {
		t.Fatalf("failed quality checks = %+v, want exactly log-level-info", failed)
	}
	if failed[0].Detail == "" {
		t.Fatal("a failed check must explain what was observed")
	}
}

// TestRetryLoopBoundedBoundsAttemptsWithoutDemandingTheFlag is the contract of
// the chain-aware rule: a fix that names only the backoff is correct (the flag
// defaults to on, the attempt budget defaults to a bounded one), while a
// policy that disables retries or keeps the amplification is not.
func TestRetryLoopBoundedBoundsAttemptsWithoutDemandingTheFlag(t *testing.T) {
	check := Check{Query: "RETRY_BACKOFF_MS", Expect: &Expectation{Min: floatPtr(20), Max: floatPtr(5)}}
	cases := []struct {
		name    string
		vars    map[string]string
		wantErr bool
	}{
		{
			name:    "storm: no backoff",
			vars:    map[string]string{"RETRY_ENABLED": "true", "RETRY_BACKOFF_MS": "0", "RETRY_ATTEMPTS": "20"},
			wantErr: true,
		},
		{
			name:    "mask: retries removed",
			vars:    map[string]string{"RETRY_ENABLED": "false", "RETRY_BACKOFF_MS": "50"},
			wantErr: true,
		},
		{
			name:    "fix: backoff only, budget left at its default",
			vars:    map[string]string{"RETRY_ENABLED": "true", "RETRY_BACKOFF_MS": "50"},
			wantErr: false,
		},
		{
			name:    "fix: bounded budget",
			vars:    map[string]string{"RETRY_ENABLED": "true", "RETRY_BACKOFF_MS": "50", "RETRY_ATTEMPTS": "5"},
			wantErr: false,
		},
		{
			name:    "amplification survives the backoff",
			vars:    map[string]string{"RETRY_ENABLED": "true", "RETRY_BACKOFF_MS": "50", "RETRY_ATTEMPTS": "20"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := PolicyRules[RuleRetryLoopBounded](context.Background(), nil, check, tc.vars)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestReadConfigFileParsesKeyValues(t *testing.T) {
	env := FakeEnvironment{
		ExecFunc: func(req runtime.ExecRequest) (runtime.ExecResult, error) {
			if req.Command[0] != "cat" || req.Command[1] != "/etc/app/app.conf" {
				return runtime.ExecResult{}, fmt.Errorf("unexpected command %v", req.Command)
			}
			return runtime.ExecResult{ExitCode: 0, Stdout: strings.Join([]string{
				"# IncidentSim service configuration.",
				"",
				"LOG_LEVEL=info",
				"LOG_FILE=\"/var/log/app/app.log\"",
				"RETRY_BACKOFF_MS = 50",
				"NOT_A_PAIR",
				"",
			}, "\n")}, nil
		},
	}

	vars, err := readConfigFile(context.Background(), env, "app", "/etc/app/app.conf")
	if err != nil {
		t.Fatalf("readConfigFile: %v", err)
	}
	want := map[string]string{
		"LOG_LEVEL":        "info",
		"LOG_FILE":         "/var/log/app/app.log",
		"RETRY_BACKOFF_MS": "50",
	}
	for key, value := range want {
		if vars[key] != value {
			t.Errorf("%s = %q, want %q", key, vars[key], value)
		}
	}
	if _, ok := vars["NOT_A_PAIR"]; ok {
		t.Error("a line without a separator became a variable")
	}
	if len(vars) != len(want) {
		t.Errorf("vars = %v, want exactly %v", vars, want)
	}
}

func TestReadConfigFileReportsUnreadableFile(t *testing.T) {
	env := FakeEnvironment{
		ExecFunc: func(req runtime.ExecRequest) (runtime.ExecResult, error) {
			return runtime.ExecResult{ExitCode: 1, Stderr: "cat: /etc/app/app.conf: No such file"}, nil
		},
	}
	if _, err := readConfigFile(context.Background(), env, "app", "/etc/app/app.conf"); err == nil {
		t.Fatal("an unreadable config file passed silently")
	}
}

// TestPolicyCheckWithFileReadsTheFileNotTheEnvironment pins the two read
// paths apart: with a declared file the check must not consult the
// environment at all, without one it must.
func TestPolicyCheckWithFileReadsTheFileNotTheEnvironment(t *testing.T) {
	env := FakeEnvironment{
		EnvValues: map[string]map[string]string{"app": {"LOG_LEVEL": "debug"}},
		ExecFunc: func(req runtime.ExecRequest) (runtime.ExecResult, error) {
			return runtime.ExecResult{ExitCode: 0, Stdout: "LOG_LEVEL=info\n"}, nil
		},
	}
	grader := NewManifestVerifier()

	withFile := grader.runCheck(context.Background(), env, Check{
		ID: "from-file", Kind: CheckPolicy, Service: "app",
		Rule: RuleConfigFlagEquals, File: "/etc/app/app.conf", Query: "LOG_LEVEL",
		Expect: &Expectation{Equals: strPtr("info")},
	})
	if !withFile.Passed {
		t.Fatalf("file-based check failed: %s", withFile.Detail)
	}
	if strings.Contains(withFile.Evidence, "LOG_LEVEL=\"debug\"") {
		t.Fatalf("evidence = %q: the file-based check read the environment", withFile.Evidence)
	}

	fromEnv := grader.runCheck(context.Background(), env, Check{
		ID: "from-env", Kind: CheckPolicy, Service: "app",
		Rule: RuleEnvFlagEquals, Query: "LOG_LEVEL",
		Expect: &Expectation{Equals: strPtr("info")},
	})
	if fromEnv.Passed {
		t.Fatal("the environment still says debug: an env-based check must fail")
	}
}
