package core

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGoBuilderContainerArgsAreBoundedAndCrossCompileForHostArch(t *testing.T) {
	builder := GoBuilder{BuildCache: "cache", Memory: "768m", CPUs: "2", Pids: "64"}
	got := builder.containerArgs()
	want := []string{
		"--memory", "768m",
		"--cpus", "2",
		"--pids-limit", "64",
		"-e", "GOOS=linux",
		"-e", "GOARCH=" + runtime.GOARCH,
		"-e", "CGO_ENABLED=0",
		"-v", "cache:/go/pkg/mod",
	}
	if len(got) != len(want) {
		t.Fatalf("containerArgs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("containerArgs() = %v, want %v", got, want)
		}
	}
}

func TestBuildTargetPrefersScenarioAppDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app", "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := buildTarget(dir)
	if err != nil || got != "./app" {
		t.Fatalf("buildTarget() = %q, %v; want ./app, nil", got, err)
	}
}
