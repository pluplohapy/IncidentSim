package core

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"incidentsim/internal/manifest"
	"incidentsim/internal/runtime"
	"incidentsim/internal/verifier"
)

// Postmortem is the draft generated from a session journal.
//
// It is intentionally a draft: the numbers come from the journal and the gates,
// the wording comes from the manifest, and what a human adds is commentary. What
// the generator must never do is hide that a symptom was removed while the root
// cause survived, so a partial fix is called out explicitly.
type Postmortem struct {
	SessionID   string    `json:"session_id"`
	TaskID      string    `json:"task_id"`
	Title       string    `json:"title"`
	Engine      string    `json:"engine"`
	GeneratedAt time.Time `json:"generated_at"`

	// StartedAt and FinishedAt bound the session.
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Duration   string    `json:"duration"`

	// Timeline is the journal in a shape a report can quote.
	Timeline []TimelineEntry `json:"timeline"`

	// RootCause is the answer key from the manifest.
	RootCause string `json:"root_cause"`
	// ReferenceSolution is the answer key from the manifest.
	ReferenceSolution manifest.ReferenceSolution `json:"reference_solution"`

	// Verdicts are the verifications the participant ran, oldest first.
	Verdicts []VerdictEntry `json:"verdicts"`

	// HintsUsed counts how much help the participant needed.
	HintsUsed int `json:"hints_used"`
	// TTD is time to the first diagnostic command; TTR is time to first pass.
	TTD         string `json:"ttd,omitempty"`
	TTR         string `json:"ttr,omitempty"`
	VerifyCount int    `json:"verify_count"`
	// AppliedFixes are the participant's changes, oldest first.
	AppliedFixes []string `json:"applied_fixes"`
	// WorkspaceDiffs are unified diffs captured at apply time.
	WorkspaceDiffs []string `json:"workspace_diffs,omitempty"`

	// PartialFix is set when the last verification passed correctness and
	// failed quality: the system works and the cause survived.
	PartialFix bool `json:"partial_fix"`
	// PartialFixNote explains what a partial fix means for this task.
	PartialFixNote string `json:"partial_fix_note,omitempty"`

	// Theory is the teaching material of the task.
	Theory string `json:"theory,omitempty"`
}

// TimelineEntry is one journal event, projected for a report.
type TimelineEntry struct {
	At      time.Time `json:"at"`
	Type    string    `json:"type"`
	Message string    `json:"message"`
}

// VerdictEntry is one verification run and its outcome.
type VerdictEntry struct {
	At          time.Time `json:"at"`
	Verdict     string    `json:"verdict"`
	Correctness string    `json:"correctness"`
	Quality     string    `json:"quality"`
	Note        string    `json:"note,omitempty"`
}

// GeneratePostmortem builds a postmortem draft from a live session.
func (o *Orchestrator) GeneratePostmortem(sessionID string) (Postmortem, error) {
	session, err := o.Sessions.Get(sessionID)
	if err != nil {
		// A finished session is still reportable: the journal survives on disk
		// even after the environment is torn down.
		return o.loadPostmortemFromJournal(sessionID)
	}
	return o.postmortemFrom(session)
}

// loadPostmortemFromJournal rebuilds what a report needs from the journal file
// alone, for a session that is no longer live.
func (o *Orchestrator) loadPostmortemFromJournal(sessionID string) (Postmortem, error) {
	journal, err := LoadJournal(o.Config.JournalDir(), sessionID)
	if err != nil {
		return Postmortem{}, err
	}
	// The task id is recorded in the session.started event payload. Parsing it
	// from the session id (everything before the first dash) is wrong for task
	// ids that themselves contain dashes, e.g. db-connection-leak -> "db".
	taskID := sessionID
	for _, event := range journal.Events() {
		if event.Type == EventSessionStarted {
			if t, ok := event.Data["task"].(string); ok && t != "" {
				taskID = t
			}
			break
		}
	}
	m, err := o.Catalog.Load(taskID)
	if err != nil {
		return Postmortem{}, fmt.Errorf("no manifest for %q: %w", taskID, err)
	}
	postmortem := postmortemSkeleton(sessionID, m)
	postmortem.StartedAt = firstEventTime(journal.Events(), EventSessionStarted)
	postmortem.Timeline = timeline(journal.Events())
	postmortem.Verdicts = verdicts(journal.Events())
	postmortem.FinishedAt = lastEventTime(journal.Events())
	postmortem.Duration = humanDuration(postmortem.StartedAt, postmortem.FinishedAt)
	postmortem.PartialFix, postmortem.PartialFixNote = partialFixFrom(journal.Events())
	postmortem.WorkspaceDiffs = workspaceDiffs(journal.Events())
	postmortem.applyStats(journal.Events())
	return postmortem, nil
}

// postmortemFrom builds the report of a live session.
func (o *Orchestrator) postmortemFrom(session *Session) (Postmortem, error) {
	postmortem := postmortemSkeleton(session.ID, session.manifest)
	postmortem.StartedAt = session.StartedAt

	events := session.journal.Events()
	postmortem.Timeline = timeline(events)
	postmortem.Verdicts = verdicts(events)
	postmortem.FinishedAt = lastEventTime(events)
	postmortem.Duration = humanDuration(postmortem.StartedAt, postmortem.FinishedAt)

	snapshot := session.Snapshot()
	postmortem.HintsUsed = snapshot.HintsUsed
	for _, patch := range snapshot.AppliedFixes {
		postmortem.AppliedFixes = append(postmortem.AppliedFixes, describePatch(patch))
	}
	postmortem.PartialFix, postmortem.PartialFixNote = partialFixFrom(events)
	postmortem.WorkspaceDiffs = workspaceDiffs(events)
	postmortem.applyStats(events)
	return postmortem, nil
}

func (p *Postmortem) applyStats(events []Event) {
	for _, event := range events {
		switch event.Type {
		case EventExecRun:
			if p.TTD == "" && !p.StartedAt.IsZero() {
				p.TTD = event.At.Sub(p.StartedAt).Round(time.Second).String()
			}
		case EventVerifyRun:
			p.VerifyCount++
			if p.TTR == "" && stringOf(event.Data["verdict"]) == verifier.VerdictPass && !p.StartedAt.IsZero() {
				p.TTR = event.At.Sub(p.StartedAt).Round(time.Second).String()
			}
		}
	}
}

func postmortemSkeleton(sessionID string, m *manifest.Manifest) Postmortem {
	engine := ""
	if m != nil {
		engine = m.Spec.Runtime.Engine
	}
	return Postmortem{
		SessionID:         sessionID,
		TaskID:            m.Metadata.ID,
		Title:             m.Metadata.Title,
		Engine:            engine,
		GeneratedAt:       time.Now().UTC(),
		RootCause:         m.Spec.RootCause.Summary,
		ReferenceSolution: m.Spec.ReferenceSolution,
		Theory:            m.Spec.Theory,
	}
}

// timeline projects the journal events of a report.
func timeline(events []Event) []TimelineEntry {
	out := make([]TimelineEntry, 0, len(events))
	for _, event := range events {
		out = append(out, TimelineEntry{At: event.At, Type: event.Type, Message: event.Message})
	}
	return out
}

// verdicts extracts the verification runs from the journal, in order. The
// verdict data lives in the event payload the orchestrator wrote.
func verdicts(events []Event) []VerdictEntry {
	var out []VerdictEntry
	for _, event := range events {
		if event.Type != EventVerifyRun {
			continue
		}
		entry := VerdictEntry{At: event.At, Verdict: stringOf(event.Data["verdict"])}
		entry.Correctness = stringOf(event.Data["correctness"])
		entry.Quality = stringOf(event.Data["quality"])
		entry.Note = stringOf(event.Data["replay"])
		out = append(out, entry)
	}
	return out
}

// partialFixFrom detects the "correctness pass, quality fail" pattern.
//
// It is the whole point of the postmortem: the generator must be able to say
// that the system works and the cause survived, rather than reporting a green
// run and leaving the reader to guess.
func partialFixFrom(events []Event) (bool, string) {
	var verifyEvents []Event
	for _, event := range events {
		if event.Type == EventVerifyRun {
			verifyEvents = append(verifyEvents, event)
		}
	}
	if len(verifyEvents) == 0 {
		return false, ""
	}
	last := verifyEvents[len(verifyEvents)-1]
	if stringOf(last.Data["correctness"]) == verifier.VerdictPass &&
		stringOf(last.Data["quality"]) == verifier.VerdictFail {
		return true, "Система работает, но корневая причина осталась: симптом устранён, причина — нет. " +
			"Quality gate при этом красный, поэтому замечание не декоративное."
	}
	return false, ""
}

func workspaceDiffs(events []Event) []string {
	var out []string
	for _, event := range events {
		if event.Type != EventApplyStarted || event.Data == nil {
			continue
		}
		if diff := stringOf(event.Data["diff"]); diff != "" {
			out = append(out, diff)
		}
	}
	return out
}

func stringOf(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func lastEventTime(events []Event) time.Time {
	var last time.Time
	for _, event := range events {
		if event.At.After(last) {
			last = event.At
		}
	}
	return last
}

func firstEventTime(events []Event, eventType string) time.Time {
	for _, event := range events {
		if event.Type == eventType {
			return event.At
		}
	}
	return time.Time{}
}

func humanDuration(from, to time.Time) string {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return "unknown"
	}
	return to.Sub(from).Round(time.Second).String()
}

// describePatch renders a participant change in one line.
func describePatch(patch runtime.EnvPatch) string {
	if len(patch.Vars) > 0 {
		keys := make([]string, 0, len(patch.Vars))
		for key, value := range patch.Vars {
			keys = append(keys, key+"="+value)
		}
		sort.Strings(keys)
		return "env " + patch.Service + ": " + strings.Join(keys, ", ")
	}
	if len(patch.Command) > 0 {
		return "statement on " + patch.Service + ": " + strings.Join(patch.Command, " ")
	}
	return "change on " + patch.Service
}

// Render renders the postmortem as markdown.
func (p Postmortem) Render() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# Постмортем: %s\n\n", p.Title))
	b.WriteString(fmt.Sprintf("- Сессия: `%s`\n- Задача: `%s`\n- Движок: %s\n- Начало: %s\n",
		p.SessionID, p.TaskID, p.Engine, p.StartedAt.Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("- Длительность: %s\n- Подсказок использовано: %d\n- Проверок: %d\n", p.Duration, p.HintsUsed, p.VerifyCount))
	if p.TTD != "" {
		b.WriteString("- TTD: " + p.TTD + "\n")
	}
	if p.TTR != "" {
		b.WriteString("- TTR: " + p.TTR + "\n")
	}
	b.WriteString("\n")

	b.WriteString("## Таймлайн\n\n")
	b.WriteString("| Время | Событие | Что произошло |\n|---|---|---|\n")
	for _, entry := range p.Timeline {
		b.WriteString(fmt.Sprintf("| %s | %s | %s |\n", entry.At.Format("15:04:05"), entry.Type, escapeCell(entry.Message)))
	}
	b.WriteString("\n")

	if len(p.Verdicts) > 0 {
		b.WriteString("## Проверки\n\n")
		b.WriteString("| Время | Итог | Correctness | Quality | Нагрузка |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for _, entry := range p.Verdicts {
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s |\n",
				entry.At.Format("15:04:05"), entry.Verdict, entry.Correctness, entry.Quality, escapeCell(entry.Note)))
		}
		b.WriteString("\n")
	}

	if len(p.AppliedFixes) > 0 {
		b.WriteString("## Изменения участника\n\n")
		for _, fix := range p.AppliedFixes {
			b.WriteString(fmt.Sprintf("- %s\n", fix))
		}
		b.WriteString("\n")
	}
	if len(p.WorkspaceDiffs) > 0 {
		b.WriteString("## Diff workspace\n\n")
		for _, diff := range p.WorkspaceDiffs {
			b.WriteString("```diff\n")
			b.WriteString(diff)
			if !strings.HasSuffix(diff, "\n") {
				b.WriteByte('\n')
			}
			b.WriteString("```\n\n")
		}
	}

	b.WriteString("## Корневая причина (эталон)\n\n")
	b.WriteString(p.RootCause + "\n\n")

	if p.PartialFix {
		b.WriteString("## Частичный фикс\n\n")
		b.WriteString(p.PartialFixNote + "\n\n")
	}

	b.WriteString("## Эталонное решение\n\n")
	b.WriteString(p.ReferenceSolution.Summary + "\n\n")
	if len(p.ReferenceSolution.Steps) > 0 {
		for i, step := range p.ReferenceSolution.Steps {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, step))
		}
		b.WriteString("\n")
	}

	if p.Theory != "" {
		b.WriteString("## Теория\n\n")
		b.WriteString(p.Theory + "\n")
	}

	b.WriteString("\n---\n\nЧерновик сгенерирован из журнала сессии. Проверьте и дополните.\n")
	return b.String()
}

func escapeCell(text string) string {
	replacer := strings.NewReplacer("|", `\|`, "\n", " ")
	return replacer.Replace(text)
}
