package webhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

func TestCreateValidatesAndDefaultsEvents(t *testing.T) {
	dispatcher := NewDispatcher()
	if _, err := dispatcher.Create(runtimeapi.WebhookCreateOptions{Name: " ", URL: "https://example.test"}); err == nil {
		t.Fatal("Create accepted an empty name")
	}
	if _, err := dispatcher.Create(runtimeapi.WebhookCreateOptions{Name: "test", URL: "file:///tmp/events"}); err == nil {
		t.Fatal("Create accepted a non-HTTP URL")
	}
	if _, err := dispatcher.Create(runtimeapi.WebhookCreateOptions{Name: "test", URL: "https://example.test", Events: []string{"unknown"}}); err == nil {
		t.Fatal("Create accepted an unsupported event")
	}
	hook, err := dispatcher.Create(runtimeapi.WebhookCreateOptions{Name: "test", URL: "https://example.test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if hook.WebhookID == "" || len(hook.Events) != 2 || hook.CreatedAt.IsZero() {
		t.Fatalf("webhook = %#v, want webhook ID, timestamps and default subscriptions", hook)
	}
	if !dispatcher.Delete(hook.WebhookID) || dispatcher.Delete(hook.WebhookID) {
		t.Fatal("Delete did not report existing then absent webhook")
	}
}

func TestPublishRetriesAndPreservesEventID(t *testing.T) {
	var calls atomic.Int32
	events := make(chan Event, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("conch-webhook-id") == "" {
			t.Errorf("headers = %#v", r.Header)
		}
		var event Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode event: %v", err)
		}
		events <- event
		if calls.Load() < 3 {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer server.Close()

	dispatcher := NewDispatcher()
	if _, err := dispatcher.Create(runtimeapi.WebhookCreateOptions{Name: "receiver", URL: server.URL, Events: []string{EventSandboxCreated}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	event, err := NewEvent(EventSandboxCreated, "sandbox-a", "", Execution{CreatedAt: "2026-08-21T10:00:00Z", VCPUNum: 2, RamMB: 512})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	dispatcher.Publish(event)
	for i := 0; i < 3; i++ {
		select {
		case received := <-events:
			if received.EventID != event.EventID || received.Version != "v1" || received.SandboxID != "sandbox-a" {
				t.Fatalf("event = %#v", received)
			}
		case <-time.After(time.Second):
			t.Fatalf("delivery %d did not arrive", i+1)
		}
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

func TestDeletePreventsSubsequentPublish(t *testing.T) {
	events := make(chan Event, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode event: %v", err)
			return
		}
		events <- event
	}))
	defer server.Close()
	dispatcher := NewDispatcher()
	hook, err := dispatcher.Create(runtimeapi.WebhookCreateOptions{Name: "receiver", URL: server.URL})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, err := NewEvent(EventSandboxCreated, "sandbox-a", "", Execution{})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	dispatcher.Publish(first)
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("first event not delivered")
	}
	if !dispatcher.Delete(hook.WebhookID) {
		t.Fatal("Delete returned false")
	}
	second, err := NewEvent(EventSandboxCreated, "sandbox-a", "", Execution{})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	dispatcher.Publish(second)
	select {
	case event := <-events:
		t.Fatalf("event delivered after deletion: %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
}
