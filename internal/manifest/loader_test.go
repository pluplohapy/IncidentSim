package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const unknownLevelManifest = `
apiVersion: incidentsim/v1
kind: Incident
metadata:
  id: fixture-task
  title: "Фикстура для тестов загрузчика"
  level: admin
spec:
  runtime:
    engine: compose
  services:
    - name: app
      image: incidentsim/workload:dev
      ports: [8080]
  fault:
    type: feature_flag
    target: app
    options:
      env:
        LOG_LEVEL: debug
  verify:
    correctness:
      - id: healthz-200
        type: http
        service: app
        path: /healthz
        expect:
          status: 200
    quality:
      - id: log-level-info
        type: policy
        service: app
        rule: env_flag_equals
        query: LOG_LEVEL
        expect:
          equals: info
        reason: "debug-логи в проде запрещены политикой"
  root_cause:
    summary: "Включён debug-уровень логирования."
    layer: config
  reference_solution:
    summary: "Понизить уровень логирования до info."
`

const validManifest = `
apiVersion: incidentsim/v1
kind: Incident
metadata:
  id: fixture-task
  title: "Фикстура для тестов загрузчика"
  level: config
spec:
  runtime:
    engine: compose
  services:
    - name: app
      image: incidentsim/workload:dev
      ports: [8080]
  fault:
    type: feature_flag
    target: app
    options:
      env:
        LOG_LEVEL: debug
  verify:
    correctness:
      - id: healthz-200
        type: http
        service: app
        path: /healthz
        expect:
          status: 200
    quality:
      - id: log-level-info
        type: policy
        service: app
        rule: env_flag_equals
        query: LOG_LEVEL
        expect:
          equals: info
        reason: "debug-логи в проде запрещены политикой"
  root_cause:
    summary: "Включён debug-уровень логирования."
    layer: config
  reference_solution:
    summary: "Понизить уровень логирования до info."
`

func TestParseAndValidateAcceptsValidManifest(t *testing.T) {
	m, err := LoadReader(strings.NewReader(validManifest))
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if m.Metadata.ID != "fixture-task" {
		t.Fatalf("id = %q, want fixture-task", m.Metadata.ID)
	}
	if got := m.Spec.Verify.Correctness[0].Method; got != "GET" {
		t.Errorf("default http method = %q, want GET", got)
	}
	if got := m.Spec.Fault.Trigger; got != TriggerOnStart {
		t.Errorf("default fault trigger = %q, want %q", got, TriggerOnStart)
	}
	if got := m.Spec.Runtime.Snapshot; got != "baseline" {
		t.Errorf("default snapshot = %q, want baseline", got)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	m, err := Parse([]byte(validManifest + "\nspec_extra: 1\n"))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	err = m.Validate()
	if err == nil {
		t.Fatal("unknown top-level field accepted")
	}
	if !strings.Contains(err.Error(), "spec_extra") {
		t.Fatalf("error does not mention the unknown field: %v", err)
	}
}

func TestValidationErrorCarriesFieldPaths(t *testing.T) {
	_, err := LoadReader(strings.NewReader(unknownLevelManifest))
	if err == nil {
		t.Fatal("unknown level accepted")
	}
	var verr *ValidationError
	if !asValidationError(err, &verr) {
		t.Fatalf("error type = %T, want *ValidationError", err)
	}
	if len(verr.Issues) == 0 {
		t.Fatal("no issues reported")
	}
	for _, issue := range verr.Issues {
		if issue.Field == "" {
			t.Errorf("issue without field: %+v", issue)
		}
		if issue.Message == "" {
			t.Errorf("issue without message: %+v", issue)
		}
	}
	if !strings.Contains(verr.Error(), "metadata.level") {
		t.Fatalf("error does not point at metadata.level: %v", verr)
	}
}

func asValidationError(err error, target **ValidationError) bool {
	verr, ok := err.(*ValidationError)
	if ok {
		*target = verr
	}
	return ok
}

func TestLoadValidCatalogFiles(t *testing.T) {
	entries, err := os.ReadDir("../../testdata/manifests/valid")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			if _, err := Load(filepath.Join("../../testdata/manifests/valid", entry.Name())); err != nil {
				t.Fatalf("valid manifest rejected: %v", err)
			}
		})
	}
}

func TestLoadRejectsInvalidCatalogFiles(t *testing.T) {
	cases := map[string]string{
		"bad-level.yaml":                "metadata.level",
		"unknown-field.yaml":            "端口",
		"unknown-fault-target.yaml":     "spec.fault",
		"quality-without-artifact.yaml": "artefact check",
		"distractor-without-check.yaml": "spec.distractors",
	}
	entries, err := os.ReadDir("../../testdata/manifests/invalid")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	seen := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		want, known := cases[entry.Name()]
		if !known {
			t.Fatalf("invalid manifest %s has no expectation in the test table", entry.Name())
		}
		seen++
		t.Run(entry.Name(), func(t *testing.T) {
			_, err := Load(filepath.Join("../../testdata/manifests/invalid", entry.Name()))
			if err == nil {
				t.Fatal("invalid manifest accepted")
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not mention %q", err, want)
			}
		})
	}
	if seen != len(cases) {
		t.Fatalf("covered %d invalid manifests, table has %d", seen, len(cases))
	}
}

func TestLoaderLoadsWholeCatalogSortedByID(t *testing.T) {
	loader := NewLoader("../../testdata/manifests/valid")
	manifests, err := loader.LoadDir()
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(manifests) != 2 {
		t.Fatalf("loaded %d manifests, want 2", len(manifests))
	}
	if manifests[0].Metadata.ID != "db-connection-leak" || manifests[1].Metadata.ID != "log-flood-disk" {
		t.Fatalf("catalog order = %s, %s", manifests[0].Metadata.ID, manifests[1].Metadata.ID)
	}
}

func TestLoaderFailsOnBrokenCatalog(t *testing.T) {
	loader := NewLoader("../../testdata/manifests/invalid")
	if _, err := loader.LoadDir(); err == nil {
		t.Fatal("a catalog with an invalid manifest must fail as a whole")
	}
}

func TestLoadTaskByID(t *testing.T) {
	loader := NewLoader("../../testdata/manifests/valid")
	m, err := loader.LoadTask("db-connection-leak")
	if err != nil {
		t.Fatalf("LoadTask: %v", err)
	}
	if m.Spec.Runtime.Engine != "compose" {
		t.Fatalf("engine = %q, want compose", m.Spec.Runtime.Engine)
	}
	if _, err := loader.LoadTask("nope"); err == nil {
		t.Fatal("unknown task accepted")
	}
	if _, err := loader.LoadTask("Nope"); err == nil {
		t.Fatal("non kebab-case id accepted")
	}
}

func TestValidateWithAssetsChecksReplayProfile(t *testing.T) {
	m, err := Load("../../testdata/manifests/valid/db-connection-leak.yaml")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if err := m.ValidateWithAssets("../../testdata"); err != nil {
		t.Fatalf("assets present but rejected: %v", err)
	}

	m.Spec.Replay.Profile = "workloads/missing.yaml"
	if err := m.ValidateWithAssets("../../testdata"); err == nil {
		t.Fatal("missing replay profile accepted")
	}
}

func TestStemMustMatchTaskID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "other-name.yaml")
	if err := os.WriteFile(path, []byte(validManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("file name that differs from metadata.id accepted")
	}
}

func TestDistractorDetectionIsRequiredInQualityGate(t *testing.T) {
	m, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatal(err)
	}
	// A distractor that nothing catches is worse than no distractor at all:
	// the task would teach the participant to mask the symptom.
	m.Spec.Distractors = []Distractor{{
		ID:             "delete-log-file",
		Title:          "Удалить файл лога",
		Symptom:        "disk-full",
		MaskedArtifact: "история логов",
		Fails:          "log-level-info",
	}}
	if err := m.Validate(); err == nil {
		t.Fatal("distractor accepted without symptoms section")
	}

	m.Spec.Symptoms = []Symptom{{ID: "disk-full", Observation: "диск заполнен"}}
	if err := m.Validate(); err != nil {
		t.Fatalf("distractor with a matching symptom rejected: %v", err)
	}
}

func TestSchemaIsReachableAndCompiles(t *testing.T) {
	if _, err := Schema(); err != nil {
		t.Fatalf("embedded schema does not compile: %v", err)
	}
}
