package core

import (
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
