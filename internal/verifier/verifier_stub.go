package verifier

import (
	"context"

	"incidentsim/internal/runtime"
)

// StubVerifier answers every verification with a fixed report. It exists so the
// core and the CLI can be built and tested before the real grader, and so a
// task graded by something other than the built-in grader (a human reviewer,
// an external grading service) can be plugged in through the same interface.
type StubVerifier struct {
	// Report is returned as-is by Verify.
	Report Report
	// Called counts the verification runs.
	Called int
}

// Verify implements Verifier.
func (s *StubVerifier) Verify(ctx context.Context, sessionID, taskID string, checks CheckSet) (Report, error) {
	s.Called++
	report := s.Report
	report.SessionID = sessionID
	report.TaskID = taskID
	return report, nil
}

// FakeEnvironment is an in-memory verifier.Environment for tests. It lets a
// check be exercised without an engine, which is what makes the
// "distractor passes correctness, fails quality" test cheap.
type FakeEnvironment struct {
	InstanceIDValue string
	// Adapter answers Runtime().
	Adapter runtime.Adapter
	// EnvValues maps service name to its effective environment.
	EnvValues map[string]map[string]string
	// SQLRows maps service name to the rows SQL() returns.
	SQLRows map[string][]map[string]string
	// ReplayText is what Replay() returns.
	ReplayText string
	// Err makes every accessor fail.
	Err error
}

// InstanceID implements Environment.
func (f FakeEnvironment) InstanceID() string { return f.InstanceIDValue }

// Runtime implements Environment.
func (f FakeEnvironment) Runtime() runtime.Adapter { return f.Adapter }

// Replay implements Environment.
func (f FakeEnvironment) Replay(ctx context.Context) (string, error) {
	return f.ReplayText, f.Err
}

// Env implements Environment.
func (f FakeEnvironment) Env(ctx context.Context, service string) (map[string]string, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.EnvValues[service], nil
}

// SQL implements Environment.
func (f FakeEnvironment) SQL(ctx context.Context, service, statement string) ([]map[string]string, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.SQLRows[service], nil
}

var _ Environment = FakeEnvironment{}
