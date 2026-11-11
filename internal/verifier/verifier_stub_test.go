package verifier

import (
	"context"
	"testing"
)

func TestReportFinalisePassesOnlyWhenBothGatesPass(t *testing.T) {
	cases := []struct {
		name        string
		correctness GateResult
		quality     GateResult
		wantCorrect string
		wantQuality string
		wantVerdict string
	}{
		{
			name:        "both green",
			correctness: gateWith(VerdictPass, true, true),
			quality:     gateWith(VerdictPass, true, true),
			wantCorrect: VerdictPass, wantQuality: VerdictPass, wantVerdict: VerdictPass,
		},
		{
			name:        "system broken",
			correctness: gateWith(VerdictFail, true, false),
			quality:     gateWith(VerdictPass, true, true),
			wantCorrect: VerdictFail, wantQuality: VerdictPass, wantVerdict: VerdictFail,
		},
		{
			name:        "symptom masked",
			correctness: gateWith(VerdictPass, true, true),
			quality:     gateWith(VerdictFail, true, false),
			wantCorrect: VerdictPass, wantQuality: VerdictFail, wantVerdict: VerdictPartial,
		},
		{
			name:        "no checks at all",
			correctness: GateResult{Gate: GateCorrectness},
			quality:     GateResult{Gate: GateQuality},
			wantCorrect: VerdictSkipped, wantQuality: VerdictSkipped, wantVerdict: VerdictFail,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := Report{Correctness: tc.correctness, Quality: tc.quality}
			report.Finalise()
			if report.Correctness.Verdict != tc.wantCorrect {
				t.Errorf("correctness = %s, want %s", report.Correctness.Verdict, tc.wantCorrect)
			}
			if report.Quality.Verdict != tc.wantQuality {
				t.Errorf("quality = %s, want %s", report.Quality.Verdict, tc.wantQuality)
			}
			if report.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %s, want %s", report.Verdict, tc.wantVerdict)
			}
		})
	}
}

func TestGateResultFailedResults(t *testing.T) {
	gate := gateWith(VerdictFail, true, false, true)
	failed := gate.FailedResults()
	if len(failed) != 1 {
		t.Fatalf("failed results = %d, want 1", len(failed))
	}
	if failed[0].Check.ID != "check-2" {
		t.Fatalf("failed check = %s, want check-2", failed[0].Check.ID)
	}
}

func TestCheckSetEmpty(t *testing.T) {
	if !(CheckSet{}).Empty() {
		t.Error("empty check set reported as non-empty")
	}
	if (CheckSet{Correctness: []Check{{ID: "x"}}}).Empty() {
		t.Error("check set with one check reported as empty")
	}
}

func TestFakeEnvironmentSatisfiesContract(t *testing.T) {
	env := FakeEnvironment{
		InstanceIDValue: "session-42",
		EnvValues:       map[string]map[string]string{"app": {"LOG_LEVEL": "info"}},
		SQLRows:         map[string][]map[string]string{"db": {{"count": "0"}}},
	}
	if env.InstanceID() != "session-42" {
		t.Errorf("InstanceID = %s", env.InstanceID())
	}
	vars, err := env.Env(context.Background(), "app")
	if err != nil || vars["LOG_LEVEL"] != "info" {
		t.Errorf("Env = %v, %v", vars, err)
	}
	rows, err := env.SQL(context.Background(), "db", "select 1")
	if err != nil || len(rows) != 1 {
		t.Errorf("SQL = %v, %v", rows, err)
	}
}

func gateWith(verdict string, outcomes ...bool) GateResult {
	gate := GateResult{Gate: GateCorrectness, Verdict: verdict}
	for i, passed := range outcomes {
		gate.Results = append(gate.Results, Result{
			Check:  Check{ID: "check-" + string(rune('1'+i)), Kind: CheckHTTP},
			Passed: passed,
		})
	}
	return gate
}
