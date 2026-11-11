package manifest

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Loader reads manifests from a catalog directory.
type Loader struct {
	// Dir is the catalog root, e.g. "manifests".
	Dir string
}

// NewLoader returns a loader rooted at dir.
func NewLoader(dir string) *Loader { return &Loader{Dir: dir} }

// Load reads, parses and validates one manifest file.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	m, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		var verr *ValidationError
		if errors.As(err, &verr) {
			verr.File = path
		}
		return nil, err
	}
	if err := checkStem(path, m); err != nil {
		return nil, err
	}
	return m, nil
}

// LoadReader is Load for streams; used by tests and by the API when a manifest
// is uploaded for validation.
func LoadReader(r io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return parseAndValidate(data)
}

// Parse unmarshals YAML into the typed manifest, applying defaults. It does not
// validate; call Manifest.Validate for that.
func Parse(data []byte) (*Manifest, error) {
	m := &Manifest{raw: data}
	if err := yaml.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	m.applyDefaults()
	return m, nil
}

// parseAndValidate is the shared entry point of Load and LoadReader.
func parseAndValidate(data []byte) (*Manifest, error) {
	m, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// LoadDir loads every manifest of a catalog directory, sorted by task id.
// A single broken manifest fails the whole call: a catalog with one invalid
// task is broken, and silently dropping tasks hides that from the operator.
func (l *Loader) LoadDir() ([]*Manifest, error) {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	out := make([]*Manifest, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !isManifestFile(entry.Name()) {
			continue
		}
		m, err := Load(filepath.Join(l.Dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("catalog %s contains no manifests", l.Dir)
	}
	// B14: cross-manifest and asset validation after every manifest loads.
	if err := validateCatalogAssets(out, filepath.Dir(l.Dir)); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadTask loads a single task by id from the catalog directory.
func (l *Loader) LoadTask(id string) (*Manifest, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	path := filepath.Join(l.Dir, id+".yaml")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("task %q not found in catalog: %w", id, err)
	}
	return Load(path)
}

func isManifestFile(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// Validate runs the semantic checks that the JSON Schema cannot express:
// cross references between manifest sections.
func (m *Manifest) Validate() error {
	document := m.raw
	if len(document) == 0 {
		document = mustMarshalYAML(m)
	}
	issues := validateAgainstSchema("", document)

	issues = append(issues, m.validateStructure()...)
	issues = append(issues, m.validateFaults()...)
	issues = append(issues, m.validateChecks()...)
	issues = append(issues, m.validateDistractors()...)

	issues = sortedIssues(issues)
	if len(issues) == 0 {
		return nil
	}
	return &ValidationError{Issues: issues}
}

func (m *Manifest) validateStructure() []Issue {
	var issues []Issue
	if m.APIVersion != APIVersion {
		issues = append(issues, Issue{Field: "apiVersion", Message: fmt.Sprintf("must be %q", APIVersion)})
	}
	if m.Kind != Kind {
		issues = append(issues, Issue{Field: "kind", Message: fmt.Sprintf("must be %q", Kind)})
	}
	if err := validateID(m.Metadata.ID); err != nil {
		issues = append(issues, Issue{Field: "metadata.id", Message: err.Error()})
	}
	if m.Metadata.Title == "" {
		issues = append(issues, Issue{Field: "metadata.title", Message: "must not be empty"})
	}
	switch m.Metadata.Level {
	case LevelConfig, LevelCode, LevelDB, LevelComposite:
	default:
		issues = append(issues, Issue{Field: "metadata.level", Message: fmt.Sprintf("must be one of config, code, db, composite")})
	}
	switch m.Spec.Runtime.Engine {
	case "compose", "firecracker", "":
	default:
		issues = append(issues, Issue{Field: "spec.runtime.engine", Message: "must be compose or firecracker"})
	}
	return issues
}

func (m *Manifest) validateFaults() []Issue {
	var issues []Issue
	services := m.serviceNames()

	if m.Spec.Fault != nil {
		issues = append(issues, validateFault("spec.fault", *m.Spec.Fault, services)...)
	}
	if len(m.Spec.FaultChain) > 0 {
		if m.Metadata.Level != LevelComposite {
			issues = append(issues, Issue{
				Field:   "spec.fault_chain",
				Message: "fault_chain requires metadata.level = composite",
			})
		}
		// Two links of the same kind are allowed in a chain as long as they
		// act at different times: a chain is a sequence over time, not a set of
		// kinds. What must not repeat is the same link twice.
		seen := map[string]bool{}
		for i, fault := range m.Spec.FaultChain {
			field := fmt.Sprintf("spec.fault_chain[%d]", i)
			issues = append(issues, validateFault(field, fault, services)...)
			key := fault.Type + "|" + fault.Trigger + "|" + fmt.Sprint(fault.Options["env"])
			if seen[key] {
				issues = append(issues, Issue{Field: field, Message: fmt.Sprintf("fault %q with trigger %q appears twice in the chain", fault.Type, fault.Trigger)})
			}
			seen[key] = true
		}
	}
	// fault is optional: code-level tasks carry the defect in the scenario
	// workspace instead of injecting it through a fault.
	return issues
}

func validateFault(field string, fault FaultSpec, services []string) []Issue {
	var issues []Issue
	switch fault.Type {
	case FaultFeatureFlag, FaultSQLConfig, FaultKill, FaultNetwork, FaultStress:
	default:
		issues = append(issues, Issue{Field: field + ".type", Message: "unknown fault type"})
	}
	if !contains(services, fault.Target) {
		issues = append(issues, Issue{
			Field:   field + ".target",
			Message: fmt.Sprintf("must reference a declared service, got %q (have %s)", fault.Target, strings.Join(services, ", ")),
		})
	}
	switch fault.Trigger {
	case "", TriggerOnStart, TriggerManual, "after_30s":
	default:
		issues = append(issues, Issue{Field: field + ".trigger", Message: "must be on_start, after_30s or manual"})
	}
	return issues
}

func (m *Manifest) validateChecks() []Issue {
	var issues []Issue
	issues = append(issues, validateReplayHooks("spec.replay", m.Spec.Replay)...)
	issues = append(issues, validateReplayHooks("spec.verify.replay", m.Spec.Verify.Replay)...)
	services := m.serviceNames()

	for gate, checks := range map[string][]Check{
		GateCorrectness: m.Spec.Verify.Correctness,
		GateQuality:     m.Spec.Verify.Quality,
	} {
		field := "spec.verify." + gate
		if len(checks) == 0 {
			issues = append(issues, Issue{Field: field, Message: "at least one check is required"})
			continue
		}
		seen := map[string]bool{}
		artifactChecks := 0
		for i, check := range checks {
			checkField := fmt.Sprintf("%s[%d]", field, i)
			if err := validateID(check.ID); err != nil {
				issues = append(issues, Issue{Field: checkField + ".id", Message: err.Error()})
			}
			if seen[check.ID] {
				issues = append(issues, Issue{Field: checkField + ".id", Message: "duplicate check id"})
			}
			seen[check.ID] = true
			if isArtifactCheck(check.Type) {
				artifactChecks++
			}
			issues = append(issues, validateCheck(checkField, check, services, gate)...)
		}
		// Quality is about production appropriateness, which only an artefact
		// can show: an env flag, a database fact, a log line. A gate made of
		// http and metric checks alone cannot tell a fix from a mask.
		if gate == GateQuality && artifactChecks == 0 {
			issues = append(issues, Issue{
				Field:   field,
				Message: "quality gate must contain at least one artefact check (policy, sql, log or exec): behaviour alone cannot tell a fix from a mask",
			})
		}
	}
	return issues
}

func validateReplayHooks(field string, replay *ReplaySpec) []Issue {
	if replay == nil {
		return nil
	}
	var issues []Issue
	for i, hook := range replay.Hooks {
		path := fmt.Sprintf("%s.hooks[%d]", field, i)
		offset, err := time.ParseDuration(hook.At)
		if err != nil || offset < 0 {
			issues = append(issues, Issue{Field: path + ".at", Message: "must be a non-negative duration"})
		}
		switch hook.Do {
		case "restart_service", "kill_service", "stub_control", "sql", "exec":
		default:
			issues = append(issues, Issue{Field: path + ".do", Message: "unknown hook action"})
		}
	}
	return issues
}

// isArtifactCheck reports whether a check inspects an artefact of the
// environment rather than its behaviour.
func isArtifactCheck(kind string) bool {
	switch kind {
	case CheckPolicy, CheckSQL, CheckLog, CheckExec:
		return true
	}
	return false
}

func validateCheck(field string, check Check, services []string, gate string) []Issue {
	var issues []Issue
	switch check.Type {
	case CheckHTTP:
		if check.Path == "" {
			issues = append(issues, Issue{Field: field, Message: "http check requires path"})
		}
		if check.Expect == nil || check.Expect.Status == nil {
			issues = append(issues, Issue{Field: field + ".expect", Message: "http check requires expect.status"})
		}
	case CheckMetric:
		if check.Query == "" {
			issues = append(issues, Issue{Field: field, Message: "metric check requires query"})
		}
		if check.Service == "" || !contains(services, check.Service) {
			issues = append(issues, Issue{Field: field + ".service", Message: "must reference a declared service"})
		}
		if check.Agg != "" {
			switch check.Agg {
			case "max", "min", "avg", "last", "p95", "slope", "delta", "resets":
			default:
				issues = append(issues, Issue{Field: field + ".agg", Message: "must be max, min, avg, last, p95, slope, delta or resets"})
			}
		}
		if check.Expect == nil || (check.Expect.Min == nil && check.Expect.Max == nil) {
			issues = append(issues, Issue{Field: field + ".expect", Message: "metric check requires expect.min or expect.max"})
		}
	case CheckSQL:
		if check.SQL == "" {
			issues = append(issues, Issue{Field: field, Message: "sql check requires sql"})
		}
		if !contains(services, check.Service) {
			issues = append(issues, Issue{Field: field + ".service", Message: "must reference a declared service"})
		}
	case CheckLog:
		if check.Pattern == "" {
			issues = append(issues, Issue{Field: field, Message: "log check requires pattern"})
		}
		if check.Service == "" || !contains(services, check.Service) {
			issues = append(issues, Issue{Field: field + ".service", Message: "must reference a declared service"})
		}
	case CheckPolicy:
		if check.Rule == "" {
			issues = append(issues, Issue{Field: field, Message: "policy check requires rule"})
		}
		if check.Reason == "" {
			issues = append(issues, Issue{Field: field + ".reason", Message: "policy check requires a reason: it must say why this is production relevant"})
		}
	case CheckExec:
		if len(check.Command) == 0 {
			issues = append(issues, Issue{Field: field, Message: "exec check requires command"})
		}
		if check.Service == "" || !contains(services, check.Service) {
			issues = append(issues, Issue{Field: field + ".service", Message: "must reference a declared service"})
		}
	case CheckReplay:
		switch check.Field {
		case "error_rate", "success_rate", "p99_ms", "rps", "errors_reset":
		default:
			issues = append(issues, Issue{Field: field + ".field", Message: "replay check field must be error_rate, success_rate, p99_ms, rps or errors_reset"})
		}
		if check.Expect == nil || (check.Expect.Min == nil && check.Expect.Max == nil) {
			issues = append(issues, Issue{Field: field + ".expect", Message: "replay check requires expect.min or expect.max"})
		}
	case CheckUnit:
		// The verifier locates hidden tests by task id; no service endpoint is
		// required for a pure-code unit check.
	default:
		issues = append(issues, Issue{Field: field + ".type", Message: "unknown check type"})
	}
	if gate == GateQuality && check.Reason == "" && check.Type != CheckPolicy {
		issues = append(issues, Issue{
			Field:   field + ".reason",
			Message: "quality checks must document why they guard production behaviour",
		})
	}
	return issues
}

func (m *Manifest) validateDistractors() []Issue {
	var issues []Issue
	qualityIDs := map[string]bool{}
	for _, check := range m.Spec.Verify.Quality {
		qualityIDs[check.ID] = true
	}
	symptomIDs := map[string]bool{}
	for _, symptom := range m.Spec.Symptoms {
		symptomIDs[symptom.ID] = true
	}

	if len(m.Spec.Distractors) > 0 && len(symptomIDs) == 0 {
		return []Issue{{
			Field:   "spec.distractors",
			Message: "distractors require spec.symptoms: a distractor hides a declared symptom",
		}}
	}

	for i, distractor := range m.Spec.Distractors {
		field := fmt.Sprintf("spec.distractors[%d]", i)
		if !qualityIDs[distractor.Fails] {
			issues = append(issues, Issue{
				Field:   field + ".fails",
				Message: fmt.Sprintf("must reference a quality check id (have %s)", joinKeys(qualityIDs)),
			})
		}
		if len(symptomIDs) > 0 && !symptomIDs[distractor.Symptom] {
			issues = append(issues, Issue{
				Field:   field + ".symptom",
				Message: fmt.Sprintf("must reference a symptom id (have %s)", joinKeys(symptomIDs)),
			})
		}
	}
	for _, id := range m.Spec.RootCause.MaskedBy {
		found := false
		for _, distractor := range m.Spec.Distractors {
			if distractor.ID == id {
				found = true
				break
			}
		}
		if !found {
			issues = append(issues, Issue{
				Field:   "spec.root_cause.masked_by",
				Message: fmt.Sprintf("references unknown distractor %q", id),
			})
		}
	}
	return issues
}

// ValidateWithAssets additionally checks that files referenced by the manifest
// exist in the repository, so a task never starts with a missing replay
// profile. Asset paths are relative to the project root, the same way they
// appear on disk.
func (m *Manifest) ValidateWithAssets(projectRoot string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	var issues []Issue
	if m.Spec.Replay != nil && m.Spec.Replay.Profile != "" {
		if !safeCatalogPath(m.Spec.Replay.Profile) {
			issues = append(issues, Issue{Field: "spec.replay.profile", Message: "must be a relative path inside the project"})
		} else {
			profile := filepath.Join(projectRoot, m.Spec.Replay.Profile)
			if _, err := os.Stat(profile); err != nil {
				issues = append(issues, Issue{
					Field:   "spec.replay.profile",
					Message: fmt.Sprintf("replay profile %s is missing", m.Spec.Replay.Profile),
				})
			}
		}
	}
	if m.Spec.Verify.Replay != nil && m.Spec.Verify.Replay.Profile != "" {
		if !safeCatalogPath(m.Spec.Verify.Replay.Profile) {
			issues = append(issues, Issue{Field: "spec.verify.replay.profile", Message: "must be a relative path inside the project"})
		} else {
			profile := filepath.Join(projectRoot, m.Spec.Verify.Replay.Profile)
			if _, err := os.Stat(profile); err != nil {
				issues = append(issues, Issue{
					Field:   "spec.verify.replay.profile",
					Message: fmt.Sprintf("replay profile %s is missing", m.Spec.Verify.Replay.Profile),
				})
			}
		}
	}
	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

func (m *Manifest) applyDefaults() {
	if m.Metadata.Level == "" {
		m.Metadata.Level = LevelConfig
	}
	if m.Spec.Runtime.Engine == "" {
		m.Spec.Runtime.Engine = "compose"
	}
	if m.Spec.Runtime.Snapshot == "" {
		m.Spec.Runtime.Snapshot = "baseline"
	}
	if m.Spec.Fault != nil && m.Spec.Fault.Trigger == "" {
		m.Spec.Fault.Trigger = TriggerOnStart
	}
	for i := range m.Spec.FaultChain {
		if m.Spec.FaultChain[i].Trigger == "" {
			m.Spec.FaultChain[i].Trigger = TriggerOnStart
		}
	}
	for i := range m.Spec.Verify.Correctness {
		if m.Spec.Verify.Correctness[i].Method == "" {
			m.Spec.Verify.Correctness[i].Method = "GET"
		}
	}
	for i := range m.Spec.Verify.Quality {
		if m.Spec.Verify.Quality[i].Method == "" {
			m.Spec.Verify.Quality[i].Method = "GET"
		}
	}
}

func (m *Manifest) serviceNames() []string {
	names := make([]string, 0, len(m.Spec.Services))
	for _, service := range m.Spec.Services {
		names = append(names, service.Name)
	}
	return names
}

// Service returns the declared service by name.
func (m *Manifest) Service(name string) (ServiceSpec, bool) {
	for _, service := range m.Spec.Services {
		if service.Name == name {
			return service, true
		}
	}
	return ServiceSpec{}, false
}

// checkStem keeps the catalog tidy: the file name must match the task id.
func checkStem(path string, m *Manifest) error {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if stem == m.Metadata.ID {
		return nil
	}
	return &ValidationError{
		File: path,
		Issues: []Issue{{
			Field:   "metadata.id",
			Message: fmt.Sprintf("file name %q must match the task id %q", stem, m.Metadata.ID),
		}},
	}
}

func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("must not be empty")
	}
	for _, part := range strings.Split(id, "-") {
		if part == "" {
			return fmt.Errorf("%q must be kebab-case without empty segments", id)
		}
		for _, r := range part {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return fmt.Errorf("%q must be lower-case kebab-case", id)
			}
		}
	}
	return nil
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func joinKeys(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ", ")
}

// mustMarshalYAML round trips the typed manifest into YAML bytes for the schema
// check. It only fails on programming errors, hence the panic-free fallback.
func mustMarshalYAML(m *Manifest) []byte {
	data, err := yaml.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return data
}

// validateCatalogAssets performs B14 cross-manifest and asset validation:
// workspace/reference must exist; editable ⊂ workspace files and present in
// reference; code-level tasks need Dockerfile/go.mod/go.sum; workloads/<id>.yaml
// must exist; hints.level must be continuous 1..N; ≥1 distractor; reference
// differs from workspace only in editable and ≤ 40 lines of diff.
func validateCatalogAssets(manifests []*Manifest, projectRoot string) error {
	var issues []Issue
	for _, m := range manifests {
		field := "task " + m.Metadata.ID

		// These are part of the task contract, not optional metadata. Keeping
		// the check here makes `validate manifests` fail before a participant
		// can start a session with an incomplete workspace.
		if m.Spec.Workspace == "" && m.Spec.Reference == "" {
			// Inline/unit-test manifests may intentionally have no editable
			// workspace. Real tasks declare both paths below.
			continue
		}
		if m.Spec.Workspace == "" {
			issues = append(issues, Issue{Field: field + ".workspace", Message: "workspace is required"})
		}
		if m.Spec.Reference == "" {
			issues = append(issues, Issue{Field: field + ".reference", Message: "reference is required"})
		}
		if m.Spec.Workspace == "" || m.Spec.Reference == "" {
			continue
		}
		if !safeCatalogPath(m.Spec.Workspace) || !safeCatalogPath(m.Spec.Reference) {
			issues = append(issues, Issue{Field: field + ".workspace", Message: "workspace and reference must be relative paths inside the project"})
			continue
		}
		workspace := filepath.Join(projectRoot, m.Spec.Workspace)
		reference := filepath.Join(projectRoot, m.Spec.Reference)
		if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
			issues = append(issues, Issue{Field: field + ".workspace", Message: fmt.Sprintf("workspace %s does not exist", m.Spec.Workspace)})
			continue
		}
		if info, err := os.Stat(reference); err != nil || !info.IsDir() {
			issues = append(issues, Issue{Field: field + ".reference", Message: fmt.Sprintf("reference %s does not exist", m.Spec.Reference)})
			continue
		}

		if m.Metadata.Level == LevelCode {
			for _, required := range []string{"Dockerfile", "go.mod", "go.sum"} {
				if _, err := os.Stat(filepath.Join(workspace, required)); os.IsNotExist(err) {
					issues = append(issues, Issue{Field: field + ".workspace", Message: fmt.Sprintf("code-level task must have workspace/%s", required)})
				}
			}
			if _, err := os.Stat(filepath.Join(projectRoot, "workloads", m.Metadata.ID+".yaml")); os.IsNotExist(err) {
				issues = append(issues, Issue{Field: field + ".verify.replay.profile", Message: fmt.Sprintf("workloads/%s.yaml is missing", m.Metadata.ID)})
			}
		}

		// editable must be a subset of workspace files and present in reference.
		workspaceFiles := map[string]bool{}
		_ = filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(workspace, path)
			workspaceFiles[rel] = true
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if spoiler := workspaceSpoiler(string(data)); spoiler != "" {
				issues = append(issues, Issue{Field: field + ".workspace", Message: fmt.Sprintf("workspace file %q contains spoiler marker %q", rel, spoiler)})
			}
			return nil
		})
		referenceFiles := map[string]bool{}
		_ = filepath.WalkDir(reference, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(reference, path)
			referenceFiles[rel] = true
			return nil
		})
		for _, editable := range m.Spec.Editable {
			if !workspaceFiles[editable] {
				issues = append(issues, Issue{Field: field + ".editable", Message: fmt.Sprintf("editable %q is not a workspace file", editable)})
			}
			if !referenceFiles[editable] {
				issues = append(issues, Issue{Field: field + ".editable", Message: fmt.Sprintf("editable %q is not a reference file", editable)})
			}
		}

		// hints.level must be continuous 1..N.
		expected := 1
		for _, hint := range m.Spec.Hints {
			if hint.Level != expected {
				issues = append(issues, Issue{Field: field + ".hints", Message: fmt.Sprintf("hint levels must be continuous 1..N, got level %d after %d", hint.Level, expected-1)})
				break
			}
			expected++
		}

		// ≥1 distractor; fails unique within the task; each has an apply action.
		if len(m.Spec.Distractors) == 0 {
			issues = append(issues, Issue{Field: field + ".distractors", Message: "at least one distractor is required"})
		}
		seenFails := map[string]bool{}
		for _, d := range m.Spec.Distractors {
			if d.Apply == nil {
				issues = append(issues, Issue{Field: field + ".distractors", Message: fmt.Sprintf("distractor %q has no apply action (B10)", d.ID)})
			}
			if seenFails[d.Fails] {
				issues = append(issues, Issue{Field: field + ".distractors", Message: fmt.Sprintf("distractors %q and another share fails %q", d.ID, d.Fails)})
			}
			seenFails[d.Fails] = true
		}

		// reference differs from workspace only in editable and ≤ 40 lines diff.
		if err := referenceDiffWithinBudget(workspace, reference, m.Spec.Editable); err != nil {
			issues = append(issues, Issue{Field: field + ".reference", Message: err.Error()})
		}
	}
	if len(issues) > 0 {
		return &ValidationError{Issues: sortedIssues(issues)}
	}
	return nil
}

// safeCatalogPath prevents a manifest loaded from a repository from escaping
// the project root when it names a workspace, reference, or replay profile.
func safeCatalogPath(path string) bool {
	if path == "" || filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// workspaceSpoiler rejects answer-bearing comments and names from a participant
// workspace. Reference material is intentionally exempt: it is never exposed
// before pass or give-up.
func workspaceSpoiler(contents string) string {
	if !utf8.ValidString(contents) {
		return ""
	}
	match := workspaceSpoilerPattern.FindStringSubmatch(strings.ToLower(contents))
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

var workspaceSpoilerPattern = regexp.MustCompile(`\b(bug|broken|fixme|leak)\b`)

// referenceDiffWithinBudget checks that reference and workspace differ only in
// editable files and that the unified diff is ≤ 40 lines.
func referenceDiffWithinBudget(workspace, reference string, editable []string) error {
	editableSet := map[string]bool{}
	for _, e := range editable {
		editableSet[e] = true
	}
	diffLines := 0
	// Reference directories are allowed to be minimal patches: files that
	// are unchanged may be omitted. Any reference file that exists must
	// still correspond to a workspace file, and differences remain limited
	// to editable files.
	if err := filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(workspace, path)
		refPath := filepath.Join(reference, rel)
		refData, refErr := os.ReadFile(refPath)
		wsData, wsErr := os.ReadFile(path)
		switch {
		case refErr != nil:
			// Unchanged files are intentionally omitted from a minimal
			// reference directory.
		case wsErr != nil:
			if !editableSet[rel] {
				return fmt.Errorf("workspace deleted non-editable file %q", rel)
			}
		default:
			if string(wsData) != string(refData) && editableSet[rel] {
				diffLines += lineDiffCount(string(refData), string(wsData))
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// Files in reference but not workspace must still be editable.
	if err := filepath.WalkDir(reference, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(reference, path)
		// SQL reference material is a database-level answer, not a
		// workspace file, so it is intentionally allowed beside the
		// participant workspace.
		if strings.HasSuffix(rel, ".sql") {
			return nil
		}
		if _, wsErr := os.ReadFile(filepath.Join(workspace, rel)); wsErr != nil {
			// A reference may carry build-only or database answer material
			// that is not part of the participant workspace.
			return nil
		}
		return nil
	}); err != nil {
		return err
	}
	if diffLines > 40 {
		return fmt.Errorf("reference diff exceeds 40 lines (%d); keep the fix minimal", diffLines)
	}
	return nil
}

// lineDiffCount counts added and removed lines in a minimal line diff. Using
// positional comparison over-counts a small insertion because every later
// line shifts; the LCS gives the same stable budget a unified diff presents.
func lineDiffCount(a, b string) int {
	alines := strings.Split(a, "\n")
	blines := strings.Split(b, "\n")
	lcs := make([][]int, len(alines)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(blines)+1)
	}
	for i := len(alines) - 1; i >= 0; i-- {
		for j := len(blines) - 1; j >= 0; j-- {
			if alines[i] == blines[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
				continue
			}
			if lcs[i+1][j] > lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	return len(alines) + len(blines) - 2*lcs[0][0]
}
