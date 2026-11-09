package verifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"incidentsim/internal/runtime"
)

// TestDistractorPassesCorrectnessButFailsQuality is the invariant that makes
// IncidentSim a simulator instead of a checklist.
//
// The participant hid the symptom: the service answers 200 and the log volume is
// gone, but the configuration still says debug. Correctness must pass, quality
// must fail, and the verdict must be fail. If this test ever goes green the
// simulator has started rewarding symptom masking.
func TestDistractorPassesCorrectnessButFailsQuality(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logPath := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logPath, []byte("2026-01-01 request served\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := FakeEnvironment{
		Services: []runtime.Service{{Name: "app", Endpoint: server.Listener.Addr().String()}},
		EnvValues: map[string]map[string]string{
			"app": {"LOG_LEVEL": "debug", "LOG_FILE": logPath},
		},
	}

	grader := NewManifestVerifier()
	report, err := grader.Verify(context.Background(), env, "session-1", "log-flood-disk", CheckSet{
		Correctness: []Check{{
			ID:      "healthz-200",
			Kind:    CheckHTTP,
			Service: "app",
			Path:    "/healthz",
			Expect:  &Expectation{Status: intPtr(200)},
		}},
		Quality: []Check{{
			ID:      "log-level-info",
			Kind:    CheckPolicy,
			Service: "app",
			Rule:    RuleEnvFlagEquals,
			Query:   "LOG_LEVEL",
			Expect:  &Expectation{Equals: strPtr("info")},
			Reason:  "debug logging is not acceptable in production",
		}},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if report.Correctness.Verdict != VerdictPass {
		t.Fatalf("correctness = %s, want pass: the symptom really is hidden", report.Correctness.Verdict)
	}
	if report.Quality.Verdict != VerdictFail {
		t.Fatalf("quality = %s, want fail: the root cause is still there", report.Quality.Verdict)
	}
	if report.Verdict != VerdictFail {
		t.Fatalf("verdict = %s, want fail", report.Verdict)
	}
	failed := report.Quality.FailedResults()
	if len(failed) != 1 || failed[0].Check.ID != "log-level-info" {
		t.Fatalf("failed quality checks = %+v, want exactly log-level-info", failed)
	}
	if failed[0].Detail == "" {
		t.Fatal("a failed check must explain what was observed")
	}
}

// TestRealFixPassesBothGates is the other half of the same invariant: after the
// configuration is actually corrected, both gates pass.
func TestRealFixPassesBothGates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logPath := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logPath, []byte("2026-01-01 request served\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := FakeEnvironment{
		Services: []runtime.Service{{Name: "app", Endpoint: server.Listener.Addr().String()}},
		EnvValues: map[string]map[string]string{
			"app": {"LOG_LEVEL": "info", "LOG_FILE": logPath},
		},
	}

	grader := NewManifestVerifier()
	report, err := grader.Verify(context.Background(), env, "session-1", "log-flood-disk", CheckSet{
		Correctness: []Check{{
			ID: "healthz-200", Kind: CheckHTTP, Service: "app", Path: "/healthz",
			Expect: &Expectation{Status: intPtr(200)},
		}},
		Quality: []Check{{
			ID: "log-level-info", Kind: CheckPolicy, Service: "app",
			Rule: RuleEnvFlagEquals, Query: "LOG_LEVEL", Expect: &Expectation{Equals: strPtr("info")},
		}},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.Passed() {
		t.Fatalf("verdict = %s, want pass\n%s", report.Verdict, report.String())
	}
}

func TestPolicyRuleEnvFlagOff(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		present bool
		wantErr bool
	}{
		{name: "unset", present: false},
		{name: "empty", value: "", present: true},
		{name: "explicit false", value: "false", present: true},
		{name: "zero", value: "0", present: true},
		{name: "still on", value: "true", present: true, wantErr: true},
		{name: "still on word", value: "yes", present: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vars := map[string]string{}
			if tc.present {
				vars["POOL_LEAK"] = tc.value
			}
			err := PolicyRules[RuleEnvFlagOff](context.Background(), nil, Check{Query: "POOL_LEAK"}, vars)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestPolicyRuleLogOutputPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PolicyRules[RuleLogOutputPreserved](context.Background(), nil, Check{Query: "LOG_FILE"}, map[string]string{"LOG_FILE": path}); err != nil {
		t.Fatalf("a present log was rejected: %v", err)
	}

	// A deleted or emptied log is the classic distractor: it hides the
	// symptom and destroys the evidence.
	empty := filepath.Join(dir, "empty.log")
	os.WriteFile(empty, nil, 0o600)
	if err := PolicyRules[RuleLogOutputPreserved](context.Background(), nil, Check{Query: "LOG_FILE"}, map[string]string{"LOG_FILE": empty}); err == nil {
		t.Error("an empty log file passed the preserved-output rule")
	}
	if err := PolicyRules[RuleLogOutputPreserved](context.Background(), nil, Check{Query: "LOG_FILE"}, map[string]string{"LOG_FILE": filepath.Join(dir, "gone.log")}); err == nil {
		t.Error("a deleted log file passed the preserved-output rule")
	}
}

func TestUnknownPolicyRuleFailsTheCheck(t *testing.T) {
	// A typo in a rule name must not silently disable the quality gate.
	grader := NewManifestVerifier()
	env := FakeEnvironment{EnvValues: map[string]map[string]string{"app": {"X": "1"}}}
	result := grader.runCheck(context.Background(), env, Check{
		ID: "typo", Kind: CheckPolicy, Service: "app", Rule: "env_flag_of", Query: "X",
	})
	if result.Passed {
		t.Fatal("an unknown policy rule passed")
	}
	if result.Detail == "" {
		t.Fatal("the failure did not name the problem")
	}
}

func TestSQLCheckParsesNumericResult(t *testing.T) {
	grader := NewManifestVerifier()
	env := FakeEnvironment{
		SQLRows: map[string][]map[string]string{
			"db": {{"column_1": "500"}},
		},
	}
	result := grader.runCheck(context.Background(), env, Check{
		ID: "max-connections", Kind: CheckSQL, Service: "db",
		SQL:    "SHOW max_connections",
		Expect: &Expectation{Max: floatPtr(200)},
	})
	if result.Passed {
		t.Fatalf("500 connections passed a limit of 200: %s", result.Detail)
	}
	if result.Evidence == "" {
		t.Fatal("no evidence recorded for the failure")
	}

	env.SQLRows["db"] = []map[string]string{{"column_1": "100"}}
	result = grader.runCheck(context.Background(), env, Check{
		ID: "max-connections", Kind: CheckSQL, Service: "db",
		SQL:    "SHOW max_connections",
		Expect: &Expectation{Max: floatPtr(200)},
	})
	if !result.Passed {
		t.Fatalf("100 connections failed a limit of 200: %s", result.Detail)
	}
}

func TestSQLCheckReportsConnectionFailure(t *testing.T) {
	grader := NewManifestVerifier()
	env := FakeEnvironment{Err: context.DeadlineExceeded}
	result := grader.runCheck(context.Background(), env, Check{
		ID: "max-connections", Kind: CheckSQL, Service: "db", SQL: "SELECT 1",
	})
	if result.Passed {
		t.Fatal("an unreachable database passed the check")
	}
}

func TestCheckWithoutEnvironmentFails(t *testing.T) {
	grader := NewManifestVerifier()
	result := grader.runCheck(context.Background(), nil, Check{ID: "x", Kind: CheckHTTP})
	if result.Passed {
		t.Fatal("a check without an environment passed")
	}
}

func floatPtr(value float64) *float64 { return &value }
