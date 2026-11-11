package selfcheck

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"incidentsim/internal/cli"
	"incidentsim/internal/verifier"
)

// verdictOrError renders a verification step for a failure message. A failed
// request leaves the report nil, and formatting the error instead of the
// verdict is the difference between a reported failure and a panic.
func TestVerdictOrErrorRendersTheVerdictOfAGoodRun(t *testing.T) {
	report := &verifier.Report{Verdict: verifier.VerdictFail}
	if got := verdictOrError(report, nil); got != "verdict=fail" {
		t.Fatalf("verdictOrError = %q, want verdict=fail", got)
	}
}

func TestVerdictOrErrorHandlesBothNil(t *testing.T) {
	if got := verdictOrError(nil, errors.New("boom")); got != "error: boom" {
		t.Fatalf("verdictOrError = %q, want error: boom", got)
	}
	if got := verdictOrError(nil, nil); got != "no report" {
		t.Fatalf("verdictOrError = %q, want no report", got)
	}
}

// The reference step used to format report.Verdict while verifyOnce had just
// returned (nil, err), so any failing /verify crashed the whole selfcheck with
// a SIGSEGV instead of reporting why the run failed.
func TestContractReportsAFailedVerifyInsteadOfCrashing(t *testing.T) {
	verifyCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/tasks":
			w.Write([]byte(`{"tasks":[{"id":"broken-task"}]}`))
		case r.URL.Path == "/api/v1/admin/tasks/broken-task":
			w.Write([]byte(`{"id":"broken-task","level":"code","editable":["app/handler.go"],` +
				`"reference_files":{"app/handler.go":"package main"},"distractors":[{"id":"d","title":"d","fails":"q"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions":
			w.Write([]byte(`{"session_id":"broken-task-1-1","id":"broken-task-1-1"}`))
		case strings.HasSuffix(r.URL.Path, "/apply"):
			w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/verify"):
			verifyCalls++
			if verifyCalls == 1 {
				// The baseline of a broken task must reach the reference step.
				w.Write([]byte(`{"verdict":"fail","correctness":{"verdict":"fail"},` +
					`"quality":{"verdict":"fail"},"task_id":"broken-task"}`))
				return
			}
			http.Error(w, "verify exploded", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	api := cli.New(server.URL)
	api.Out = &strings.Builder{}
	api.Err = &strings.Builder{}
	res, err := ContractForEngine(context.Background(), api, "broken-task", "compose")
	if err == nil {
		t.Fatal("a failing reference verify must report an error, not pass")
	}
	if res == nil {
		t.Fatal("ContractForEngine returned no result")
	}
	if res.PassOK {
		t.Error("PassOK = true, want false: the verify call failed")
	}
	if !strings.Contains(res.Err.Error(), "reference verify") {
		t.Fatalf("error = %v, want it to name the reference verify step", res.Err)
	}
	if !strings.Contains(res.Err.Error(), "verify exploded") {
		t.Fatalf("error = %v, want it to carry the core error", res.Err)
	}
}
