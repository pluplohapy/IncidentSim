package core

import (
	"strings"
	"testing"
	"time"

	"incidentsim/internal/runtime"
	"incidentsim/internal/verifier"
)

// journalWith builds a journal exactly the way the orchestrator writes it, so
// the postmortem is tested against real event shapes.
func journalWith(t *testing.T, events []Event) *Journal {
	t.Helper()
	journal := NewJournal("task-1")
	for _, event := range events {
		if err := journal.Record(event.Type, event.Message, event.Data); err != nil {
			t.Fatalf("record %s: %v", event.Type, err)
		}
	}
	return journal
}

func verifyEvent(verdict, correctness, quality, replay string) Event {
	return Event{Type: EventVerifyRun, Message: "verify", Data: map[string]any{
		"verdict":     verdict,
		"correctness": correctness,
		"quality":     quality,
		"replay":      replay,
	}}
}

func TestPartialFixIsDetectedFromTheJournal(t *testing.T) {
	// The generator must be able to say "the system works, the cause survived":
	// that sentence is the difference between a report and a green light.
	journal := journalWith(t, []Event{
		{Type: EventSessionStarted, Message: "session started"},
		verifyEvent(verifier.VerdictFail, verifier.VerdictFail, verifier.VerdictFail, "replay: 10 errors"),
		verifyEvent(verifier.VerdictFail, verifier.VerdictPass, verifier.VerdictFail, "replay: 0 errors"),
	})

	partial, note := partialFixFrom(journal.Events())
	if !partial {
		t.Fatal("a correctness pass with a quality fail was not reported as a partial fix")
	}
	if !strings.Contains(note, "корневая причина") {
		t.Fatalf("note = %q, want an explanation of the surviving cause", note)
	}
}

func TestFullFixIsNotAPartialFix(t *testing.T) {
	journal := journalWith(t, []Event{
		verifyEvent(verifier.VerdictPass, verifier.VerdictPass, verifier.VerdictPass, "replay: 0 errors"),
	})
	if partial, _ := partialFixFrom(journal.Events()); partial {
		t.Fatal("a passing run was reported as a partial fix")
	}
}

func TestBrokenRunIsNotAPartialFix(t *testing.T) {
	journal := journalWith(t, []Event{
		verifyEvent(verifier.VerdictFail, verifier.VerdictFail, verifier.VerdictFail, "replay: 500 errors"),
	})
	if partial, _ := partialFixFrom(journal.Events()); partial {
		t.Fatal("a still-broken run was reported as a partial fix")
	}
}

func TestTimelineAndVerdictsAreExtracted(t *testing.T) {
	journal := journalWith(t, []Event{
		{Type: EventSessionStarted, Message: "session started"},
		{Type: EventEnvironmentUp, Message: "environment is up"},
		{Type: EventFaultActivated, Message: "fault activated by injector"},
		verifyEvent(verifier.VerdictFail, verifier.VerdictFail, verifier.VerdictFail, "replay: 12 errors"),
		{Type: EventFixApplied, Message: "participant applied a change", Data: map[string]any{"vars": map[string]any{"LOG_LEVEL": "info"}}},
	})

	if entries := timeline(journal.Events()); len(entries) != 5 {
		t.Fatalf("timeline = %d entries, want 5", len(entries))
	}
	runs := verdicts(journal.Events())
	if len(runs) != 1 {
		t.Fatalf("verdicts = %d, want 1", len(runs))
	}
	if runs[0].Correctness != verifier.VerdictFail || runs[0].Note != "replay: 12 errors" {
		t.Fatalf("verdict entry = %+v", runs[0])
	}
}

func TestRenderIncludesPartialFixCallout(t *testing.T) {
	postmortem := Postmortem{
		SessionID: "task-1-120000",
		Title:     "Диск заполнен логами",
		RootCause: "debug-логи без ротации",
		StartedAt: time.Now().UTC().Add(-10 * time.Minute),
		Timeline: []TimelineEntry{
			{At: time.Now().UTC(), Type: EventFaultActivated, Message: "fault activated"},
			{At: time.Now().UTC(), Type: EventFixApplied, Message: "applied LOG_LEVEL=info"},
		},
		Verdicts:       []VerdictEntry{{At: time.Now().UTC(), Verdict: verifier.VerdictPartial, Correctness: verifier.VerdictPass, Quality: verifier.VerdictFail, Note: "replay: 0 errors"}},
		PartialFix:     true,
		PartialFixNote: "Система работает, но корневая причина осталась.",
	}

	rendered := postmortem.Render()
	for _, want := range []string{"# Постмортем", "## Таймлайн", "## Проверки", "## Частичный фикс", "## Корневая причина"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render is missing %q:\n%s", want, rendered)
		}
	}
	// A report that never states the outcome is worse than no report.
	if !strings.Contains(rendered, "Система работает") {
		t.Fatal("the partial-fix callout is missing from the report")
	}
}

func TestRenderEscapesTableCells(t *testing.T) {
	if got := escapeCell("a|b\nc"); !strings.Contains(got, `\|`) || strings.Contains(got, "\n") {
		t.Fatalf("escapeCell = %q", got)
	}
}

func TestDescribePatch(t *testing.T) {
	if got := describePatch(runtime.EnvPatch{Service: "app", Vars: map[string]string{"LOG_LEVEL": "info"}}); got != "env app: LOG_LEVEL=info" {
		t.Fatalf("env patch = %q", got)
	}
	if got := describePatch(runtime.EnvPatch{Service: "db", Command: []string{"VACUUM"}}); !strings.Contains(got, "statement on db") {
		t.Fatalf("statement patch = %q", got)
	}
}

func TestHumanDuration(t *testing.T) {
	start := time.Now().UTC().Add(-5 * time.Minute)
	if got := humanDuration(start, start.Add(5*time.Minute)); got != "5m0s" {
		t.Fatalf("duration = %q", got)
	}
	if got := humanDuration(time.Time{}, time.Now()); got != "unknown" {
		t.Fatalf("duration of an unbounded session = %q", got)
	}
}
