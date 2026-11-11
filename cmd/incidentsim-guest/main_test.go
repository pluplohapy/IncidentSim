package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestAgent returns an agent whose service is already running with the
// given environment, served over plain HTTP so the test needs no vsock.
func newTestAgent(t *testing.T, env map[string]string) *agent {
	t.Helper()
	a := &agent{procs: map[string]*serviceProcess{}, started: time.Now().UTC(), configPath: os.Getenv("CONFIG_PATH")}
	a.config = guestConfig{Services: []serviceConfig{{
		Name:    "app",
		Command: []string{"/bin/sh", "-c", "echo guest-process-ok; sleep 5"},
		Env:     env,
		Port:    8080,
	}}}
	if err := a.start(a.config.Services[0]); err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(func() { a.procs["app"].cmd.Process.Kill() })
	return a
}

func TestGuestAgentReportsServiceEnvironment(t *testing.T) {
	a := newTestAgent(t, map[string]string{"LOG_LEVEL": "debug", "POOL_LEAK": "true"})
	server := httptest.NewServer(a.handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/env?service=app")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env["LOG_LEVEL"] != "debug" {
		t.Fatalf("env = %v, want LOG_LEVEL=debug", env)
	}
}

func TestGuestAgentRestartAppliesEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CONFIG_PATH", filepath.Join(dir, "config.json"))

	a := newTestAgent(t, map[string]string{"LOG_LEVEL": "debug"})
	server := httptest.NewServer(a.handler())
	defer server.Close()

	// The fix: a policy check must see the new value after a restart, and the
	// configuration on disk must reflect it so a snapshot restore keeps it.
	body := strings.NewReader(`{"service":"app","env":{"LOG_LEVEL":"info"}}`)
	resp, err := http.Post(server.URL+"/restart", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restart status = %d", resp.StatusCode)
	}

	getResp, err := http.Get(server.URL + "/env?service=app")
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	var env map[string]string
	json.NewDecoder(getResp.Body).Decode(&env)
	if env["LOG_LEVEL"] != "info" {
		t.Fatalf("env after restart = %v, want LOG_LEVEL=info", env)
	}

	stored, err := loadConfig(os.Getenv("CONFIG_PATH"))
	if err != nil {
		t.Fatalf("config was not persisted: %v", err)
	}
	if stored.Services[0].Env["LOG_LEVEL"] != "info" {
		t.Fatalf("persisted env = %v", stored.Services[0].Env)
	}
}

func TestGuestAgentRestartUnsetsEmptyValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CONFIG_PATH", filepath.Join(dir, "config.json"))

	a := newTestAgent(t, map[string]string{"POOL_LEAK": "true"})
	server := httptest.NewServer(a.handler())
	defer server.Close()

	body := strings.NewReader(`{"service":"app","env":{"POOL_LEAK":""}}`)
	resp, err := http.Post(server.URL+"/restart", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	config, err := loadConfig(os.Getenv("CONFIG_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	if _, present := config.Services[0].Env["POOL_LEAK"]; present {
		t.Fatalf("empty value did not unset the variable: %v", config.Services[0].Env)
	}
}

func TestGuestAgentRejectsUnknownService(t *testing.T) {
	a := newTestAgent(t, nil)
	server := httptest.NewServer(a.handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/env?service=nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGuestAgentLogsAreBounded(t *testing.T) {
	a := newTestAgent(t, nil)
	server := httptest.NewServer(a.handler())
	defer server.Close()

	logs := newRingLog(3)
	for _, line := range []string{"one", "two", "three", "four"} {
		logs.write(line)
	}
	got := logs.read()
	if len(got) != 3 || got[0] != "two" {
		t.Fatalf("ring log kept %v, want the last three lines", got)
	}
}

func TestGuestAgentHealthz(t *testing.T) {
	a := newTestAgent(t, nil)
	server := httptest.NewServer(a.handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestParseEnvOutput(t *testing.T) {
	env := map[string]string{}
	for _, line := range strings.Split("PATH=/bin\nLOG_LEVEL=debug\nBROKEN\n", "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			continue
		}
		env[key] = value
	}
	if env["PATH"] != "/bin" || env["LOG_LEVEL"] != "debug" {
		t.Fatalf("env = %v", env)
	}
	if _, ok := env["BROKEN"]; ok {
		t.Fatal("a line without '=' was parsed as a variable")
	}
}
