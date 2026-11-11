package teleport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"incidentsim/internal/core"
)

func TestStubKeepsRecordingsAndNeverFails(t *testing.T) {
	stub := NewStub()
	record := core.SessionRecord{SessionID: "s-1", TaskID: "log-flood-disk", StartedAt: time.Now().UTC()}
	if err := stub.Record(context.Background(), record); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := stub.Records(); len(got) != 1 || got[0].SessionID != "s-1" {
		t.Fatalf("records = %+v", got)
	}
	// The same record twice is a retry, not a duplicate: the operator can
	// export the set as it is.
	if err := stub.Record(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if len(stub.Records()) != 2 {
		t.Fatalf("records = %d, want 2", len(stub.Records()))
	}
}

func TestFromConfigReturnsStubWhenDisabled(t *testing.T) {
	if _, ok := FromConfig(Config{}).(*Stub); !ok {
		t.Fatal("a disabled audit must degrade to the stub")
	}
	// Even a broken configuration degrades: audit is an obligation, not a gate.
	if _, ok := FromConfig(Config{Enabled: true, Endpoint: ""}).(*Stub); !ok {
		t.Fatal("a misconfigured audit must degrade to the stub")
	}
}

func TestClientUploadsRecordings(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = readAll(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := New(Config{Enabled: true, Endpoint: server.URL, JoinToken: "token-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	record := core.SessionRecord{SessionID: "s-9", TaskID: "db-connection-leak"}
	if err := client.Record(context.Background(), record); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if gotPath != "/v1/webapi/sessions/record" {
		t.Fatalf("upload path = %q", gotPath)
	}
	if gotAuth != "Bearer token-1" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if !strings.Contains(string(gotBody), "db-connection-leak") {
		t.Fatalf("uploaded body = %q", gotBody)
	}
}

func TestClientReportsUploadFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, _ := New(Config{Enabled: true, Endpoint: server.URL})
	if err := client.Record(context.Background(), core.SessionRecord{SessionID: "s"}); err == nil {
		t.Fatal("an upload failure was not reported")
	}
}

func TestStubSatisfiesTheCoreContract(t *testing.T) {
	// The core assigns the adapter to a core.AuditRecorder field, so the
	// compile-time check lives here rather than in the core test.
	var recorder core.AuditRecorder = NewStub()
	if recorder.Name() != "teleport-stub" {
		t.Fatalf("name = %q", recorder.Name())
	}
	if err := recorder.Record(context.Background(), core.SessionRecord{SessionID: "s"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func TestNewRequiresAnEndpoint(t *testing.T) {
	if _, err := New(Config{Enabled: true}); err == nil {
		t.Fatal("a client without an endpoint was accepted")
	}
}

func TestClientNameAndTimeoutDefaults(t *testing.T) {
	client, err := New(Config{Endpoint: "teleport.example.com:3080"})
	if err != nil {
		t.Fatal(err)
	}
	if client.Name() != "teleport" {
		t.Fatalf("name = %q", client.Name())
	}
	if client.HTTP.Timeout <= 0 {
		t.Fatal("no default timeout: an unreachable Teleport would hang the caller")
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buffer := make([]byte, 0, 512)
	for {
		chunk := make([]byte, 512)
		read, err := r.Body.Read(chunk)
		if read > 0 {
			buffer = append(buffer, chunk[:read]...)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return buffer, nil
			}
			return buffer, nil
		}
	}
}
