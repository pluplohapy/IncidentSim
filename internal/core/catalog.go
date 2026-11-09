package core

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"incidentsim/internal/manifest"
	"incidentsim/internal/runtime"
	"incidentsim/internal/verifier"
)

// TaskSummary is the catalog listing entry the CLI and the API expose.
type TaskSummary struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Level            string   `json:"level"`
	Tags             []string `json:"tags,omitempty"`
	EstimatedMinutes int      `json:"estimated_minutes,omitempty"`
	Engine           string   `json:"engine"`
	Correctness      int      `json:"correctness_checks"`
	Quality          int      `json:"quality_checks"`
	Distractors      int      `json:"distractors"`
	Path             string   `json:"-"`
}

// Catalog is the set of tasks the deployment offers. It is loaded once at
// startup and reloaded on demand, so adding a manifest does not require a
// restart of the core.
type Catalog struct {
	mu      sync.RWMutex
	loader  *manifest.Loader
	byID    map[string]*manifest.Manifest
	ordered []string
}

// NewCatalog loads every manifest of dir. A broken manifest fails the whole
// catalog: a catalog that silently drops tasks lies to the operator.
func NewCatalog(dir string) (*Catalog, error) {
	catalog := &Catalog{loader: manifest.NewLoader(dir), byID: map[string]*manifest.Manifest{}}
	if err := catalog.Reload(); err != nil {
		return nil, err
	}
	return catalog, nil
}

// Reload re-reads the catalog directory.
func (c *Catalog) Reload() error {
	manifests, err := c.loader.LoadDir()
	if err != nil {
		return err
	}
	byID := make(map[string]*manifest.Manifest, len(manifests))
	for _, m := range manifests {
		byID[m.Metadata.ID] = m
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byID = byID
	c.ordered = make([]string, 0, len(manifests))
	for _, m := range manifests {
		c.ordered = append(c.ordered, m.Metadata.ID)
	}
	sort.Strings(c.ordered)
	return nil
}

// Load returns the manifest of a task.
func (c *Catalog) Load(id string) (*manifest.Manifest, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.byID[id]
	if !ok {
		return nil, fmt.Errorf("unknown task %q (have %s)", id, c.idsLocked())
	}
	return m, nil
}

// List returns the catalog summaries in id order.
func (c *Catalog) List() []TaskSummary {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]TaskSummary, 0, len(c.ordered))
	for _, id := range c.ordered {
		out = append(out, c.summariseLocked(c.byID[id]))
	}
	return out
}

// IDs returns the task ids in order.
func (c *Catalog) IDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string(nil), c.ordered...)
}

func (c *Catalog) summariseLocked(m *manifest.Manifest) TaskSummary {
	return TaskSummary{
		ID:               m.Metadata.ID,
		Title:            m.Metadata.Title,
		Level:            m.Metadata.Level,
		Tags:             m.Metadata.Tags,
		EstimatedMinutes: m.Metadata.EstimatedMinutes,
		Engine:           m.Spec.Runtime.Engine,
		Correctness:      len(m.Spec.Verify.Correctness),
		Quality:          len(m.Spec.Verify.Quality),
		Distractors:      len(m.Spec.Distractors),
		Path:             "",
	}
}

func (c *Catalog) idsLocked() string {
	if len(c.ordered) == 0 {
		return "none"
	}
	return fmt.Sprint(c.ordered)
}

// Checks projects the manifest verification into the verifier contract. The
// projection lives here so that a schema change is a change of one function.
func Checks(m *manifest.Manifest) verifier.CheckSet {
	set := verifier.CheckSet{
		Correctness: projectChecks(m.Spec.Verify.Correctness, verifier.GateCorrectness),
		Quality:     projectChecks(m.Spec.Verify.Quality, verifier.GateQuality),
	}
	if m.Spec.Replay != nil {
		set.Replay = &verifier.ReplayConfig{
			Profile:     m.Spec.Replay.Profile,
			Duration:    m.Spec.Replay.Duration,
			RPS:         m.Spec.Replay.RPS,
			Concurrency: m.Spec.Replay.Concurrency,
		}
	}
	return set
}

func projectChecks(checks []manifest.Check, gate verifier.Gate) []verifier.Check {
	out := make([]verifier.Check, 0, len(checks))
	for _, check := range checks {
		out = append(out, verifier.Check{
			ID:       check.ID,
			Kind:     verifier.CheckKind(check.Type),
			Gate:     gate,
			Title:    check.Title,
			Service:  check.Service,
			Path:     check.Path,
			Method:   check.Method,
			Query:    check.Query,
			SQL:      check.SQL,
			Pattern:  check.Pattern,
			Rule:     check.Rule,
			Command:  check.Command,
			Duration: check.Duration,
			Expect:   projectExpectation(check.Expect),
			Reason:   check.Reason,
		})
	}
	return out
}

func projectExpectation(expect *manifest.Expectation) *verifier.Expectation {
	if expect == nil {
		return nil
	}
	return &verifier.Expectation{
		Status:  expect.Status,
		Equals:  expect.Equals,
		Matches: expect.Matches,
		Min:     expect.Min,
		Max:     expect.Max,
	}
}

// ParseEnvPatch turns repeated `--service app --set KEY=VALUE` flags into a
// patch. It is in the core, not in the CLI, so the HTTP API and the CLI share
// one definition of what a fix looks like.
func ParseEnvPatch(service string, sets []string, restart bool) (runtime.EnvPatch, error) {
	patch := runtime.EnvPatch{Service: service, Restart: restart, Vars: map[string]string{}}
	for _, set := range sets {
		key, value, ok := strings.Cut(set, "=")
		if !ok || key == "" {
			return patch, fmt.Errorf("invalid --set %q, want KEY=VALUE", set)
		}
		patch.Vars[key] = value
	}
	if len(patch.Vars) == 0 {
		return patch, fmt.Errorf("no --set given: a fix must change at least one variable")
	}
	return patch, nil
}
