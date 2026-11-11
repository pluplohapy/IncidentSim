package main

import (
	"os"
	"path/filepath"
	"testing"

	"incidentsim/internal/core"
)

func TestCatalogDistractorsHaveApplyAndUniqueFails(t *testing.T) {
	catalog, err := core.NewCatalog(filepath.Join("..", "..", "manifests"))
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	// Every distractor must have an apply action (data-driven, B10) and a
	// unique fails value within the task.
	for _, id := range catalog.IDs() {
		task, err := catalog.Load(id)
		if err != nil {
			t.Fatalf("load %s: %v", id, err)
		}
		qualityIDs := map[string]bool{}
		for _, check := range task.Spec.Verify.Quality {
			qualityIDs[check.ID] = true
		}
		byFails := map[string]string{}
		for _, d := range task.Spec.Distractors {
			if !qualityIDs[d.Fails] {
				t.Errorf("%s: distractor %s is graded by %q, which is not a quality check of the task", id, d.ID, d.Fails)
			}
			if d.Apply == nil {
				t.Errorf("%s: distractor %s has no apply action (B10: distractors[].apply is required)", id, d.ID)
			}
			if other, dup := byFails[d.Fails]; dup {
				t.Errorf("%s: distractors %s and %s both fail %q: they would apply the same action", id, other, d.ID, d.Fails)
			}
			byFails[d.Fails] = d.ID
		}
	}
}

func setenv(key, value string) {
	_ = os.Setenv(key, value)
}
