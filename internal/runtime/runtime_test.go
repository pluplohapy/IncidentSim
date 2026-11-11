package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFakeAdapterRecordsCallOrder(t *testing.T) {
	adapter := NewFake("compose")
	ctx := context.Background()

	instance, err := adapter.Up(ctx, "session-1", Spec{})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if instance.ID != "session-1" || instance.Engine != "compose" {
		t.Fatalf("instance = %+v", instance)
	}
	if len(instance.Services) != 1 || instance.Services[0].Name != "app" {
		t.Fatalf("services = %+v", instance.Services)
	}

	if _, err := adapter.Exec(ctx, "session-1", ExecRequest{Service: "app", Command: []string{"ls"}}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if _, err := adapter.Logs(ctx, "session-1", LogOptions{Service: "app"}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if _, err := adapter.Metrics(ctx, "session-1", "app"); err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if _, err := adapter.Apply(ctx, "session-1", EnvPatch{Service: "app", Vars: map[string]string{"LOG_LEVEL": "info"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := adapter.Reset(ctx, "session-1"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	want := []string{"Up", "Exec:app", "Logs:app", "Metrics:app", "Apply:app", "Reset"}
	if got := adapter.Calls; len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if adapter.Calls[i] != want[i] {
			t.Fatalf("call %d = %s, want %s (all: %v)", i, adapter.Calls[i], want[i], want)
		}
	}
}

func TestFakeAdapterTracksAppliedEnvironment(t *testing.T) {
	adapter := NewFake("compose")
	ctx := context.Background()

	if _, err := adapter.Apply(ctx, "s", EnvPatch{Service: "app", Vars: map[string]string{"LOG_LEVEL": "debug"}}); err != nil {
		t.Fatal(err)
	}
	if value, ok := adapter.LastEnv("app", "LOG_LEVEL"); !ok || value != "debug" {
		t.Fatalf("LastEnv = %q, %v", value, ok)
	}

	if _, err := adapter.Apply(ctx, "s", EnvPatch{Service: "app", Vars: map[string]string{"LOG_LEVEL": "info"}}); err != nil {
		t.Fatal(err)
	}
	if value, ok := adapter.LastEnv("app", "LOG_LEVEL"); !ok || value != "info" {
		t.Fatalf("LastEnv after fix = %q, %v", value, ok)
	}

	// An empty value means the variable is unset, i.e. the flag is off.
	if _, err := adapter.Apply(ctx, "s", EnvPatch{Service: "app", Vars: map[string]string{"POOL_LEAK": ""}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := adapter.LastEnv("app", "POOL_LEAK"); ok {
		t.Fatal("empty patch value still reported as set")
	}

	if _, err := adapter.Reset(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if len(adapter.AppliedPatches()) != 0 {
		t.Fatalf("reset kept patches: %v", adapter.AppliedPatches())
	}
}

func TestFakeAdapterPropagatesFailures(t *testing.T) {
	adapter := NewFake("compose-test")
	adapter.FailWith = ErrTimeout
	ctx := context.Background()

	if _, err := adapter.Up(ctx, "s", Spec{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Up error = %v", err)
	}
	if err := adapter.Down(ctx, "s"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Down error = %v", err)
	}
}

func TestWaitForHealthPollsUntilEndpointAnswers(t *testing.T) {
	// The first connections are dropped without a response, which is what a
	// service still booting looks like; the poll must keep trying.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	attempts := 0
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			attempts++
			if attempts < 3 {
				conn.Close() // drop the connection, no HTTP response
				continue
			}
			fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			conn.Close()
		}
	}()

	service := Service{
		Name:        "app",
		Endpoint:    listener.Addr().String(),
		Healthcheck: &HealthcheckSpec{Path: "/healthz", IntervalSeconds: 1, TimeoutSeconds: 1},
	}
	if err := waitEndpoint(context.Background(), service, service.Healthcheck, 5*time.Second); err != nil {
		t.Fatalf("waitEndpoint: %v", err)
	}
	if attempts < 3 {
		t.Fatalf("gave up after %d attempts", attempts)
	}
}

func TestWaitForHealthGivesUpOnDeadServer(t *testing.T) {
	// A service that is not listening at all is not usable, whatever its
	// status would be: this is the failure Up must report.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close()
	_ = url

	service := Service{Name: "app", Healthcheck: &HealthcheckSpec{Path: "/healthz", IntervalSeconds: 1, TimeoutSeconds: 1}}
	// Point at a port nobody listens on.
	dead := Service{Name: "app", Endpoint: "127.0.0.1:1", Healthcheck: service.Healthcheck}
	start := time.Now()
	err := waitEndpoint(context.Background(), dead, dead.Healthcheck, 300*time.Millisecond)
	if err == nil {
		t.Fatal("a service that does not listen was accepted as healthy")
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("gave up too early: %v", elapsed)
	}
}

func TestWaitForHealthAcceptsDegradedService(t *testing.T) {
	// An incident environment is allowed to answer 5xx: the fault is the
	// point, and the adapter must not block on it.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	service := Service{Name: "app", Healthcheck: &HealthcheckSpec{Path: "/healthz", IntervalSeconds: 1, TimeoutSeconds: 1}}
	if err := waitEndpoint(context.Background(), endpoint(server, service), service.Healthcheck, 2*time.Second); err != nil {
		t.Fatalf("a degraded but responsive service was rejected: %v", err)
	}
}

func TestWaitForHealthSkipsServicesWithoutCheck(t *testing.T) {
	instance := &Instance{Services: []Service{{Name: "db"}}}
	if err := WaitForHealth(context.Background(), instance, time.Millisecond); err != nil {
		t.Fatalf("WaitForHealth: %v", err)
	}
}

func endpoint(server *httptest.Server, service Service) Service {
	service.Endpoint = server.Listener.Addr().String()
	return service
}
