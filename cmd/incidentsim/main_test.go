package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"incidentsim/internal/cli"
	"incidentsim/internal/core"
)

func TestSplitSQLStatements(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "two statements",
			in:   "ALTER TABLE orders SET (autovacuum_enabled = true);\nVACUUM (VERBOSE, ANALYZE) orders;",
			want: []string{
				"ALTER TABLE orders SET (autovacuum_enabled = true)",
				"VACUUM (VERBOSE, ANALYZE) orders",
			},
		},
		{
			name: "comments and blanks dropped",
			in:   "-- enable autovacuum\nALTER TABLE orders SET (autovacuum_enabled = true);\n\n",
			want: []string{"ALTER TABLE orders SET (autovacuum_enabled = true)"},
		},
		{
			name: "empty",
			in:   "\n\n-- nothing\n",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitSQLStatements(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

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

func TestSelfcheckRejectsUnknownTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/tasks" {
			_, _ = w.Write([]byte(`{"tasks":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	setenv("INCIDENTSIM_ADDR", server.URL)

	err := selfcheck(context.Background(), cli.New(server.URL), "nonexistent")
	if err == nil || !strings.Contains(err.Error(), "not in the catalog") {
		t.Fatalf("selfcheck error = %v, want 'not in the catalog'", err)
	}
}

func TestSelfcheckUsesCatalogEntry(t *testing.T) {
	var started bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/tasks":
			_, _ = w.Write([]byte(`{"tasks":[{"id":"fixture","level":"config","editable":["config/app.conf"]}]}`))
		case r.URL.Path == "/api/v1/tasks/fixture":
			_, _ = w.Write([]byte(`{"id":"fixture","level":"config","editable":["config/app.conf"],"distractors":[{"id":"d1","title":"distractor1","fails":"log-history-preserved"}]}`))
		case r.URL.Path == "/api/v1/sessions" && r.Method == http.MethodPost:
			started = true
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"engine unavailable"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	setenv("INCIDENTSIM_ADDR", server.URL)

	err := selfcheck(context.Background(), cli.New(server.URL), "fixture")
	if err == nil || !strings.Contains(err.Error(), "start") {
		t.Fatalf("selfcheck error = %v, want start failure", err)
	}
	if !started {
		t.Fatal("selfcheck did not attempt to start the task")
	}
}

func TestSelfcheckFailsWhenTaskHasNoDistractors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/tasks":
			_, _ = w.Write([]byte(`{"tasks":[{"id":"fixture","level":"config","editable":["config/app.conf"]}]}`))
		case r.URL.Path == "/api/v1/tasks/fixture":
			_, _ = w.Write([]byte(`{"id":"fixture","level":"config","editable":["config/app.conf"],"distractors":[]}`))
		case r.URL.Path == "/api/v1/sessions" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"fixture-1-120000","state":"running"}`))
		case strings.HasSuffix(r.URL.Path, "/apply"):
			_, _ = w.Write([]byte(`{"applied":{}}`))
		case strings.HasSuffix(r.URL.Path, "/verify"):
			_, _ = w.Write([]byte(`{"verdict":"pass","correctness":{"verdict":"pass"},"quality":{"verdict":"pass"}}`))
		case strings.HasSuffix(r.URL.Path, "/reset"):
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/stop"):
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	setenv("INCIDENTSIM_ADDR", server.URL)

	err := selfcheck(context.Background(), cli.New(server.URL), "fixture")
	if err == nil || !strings.Contains(err.Error(), "no distractors") {
		t.Fatalf("selfcheck error = %v, want 'no distractors'", err)
	}
}

func setenv(key, value string) {
	_ = os.Setenv(key, value)
}
