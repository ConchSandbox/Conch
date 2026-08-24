package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openeuler/Conch/internal/id"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	EventSandboxCreated = "sandbox.lifecycle.created"
	EventSandboxKilled  = "sandbox.lifecycle.killed"
)

var supportedEvents = map[string]struct{}{
	EventSandboxCreated: {},
	EventSandboxKilled:  {},
}

type Execution struct {
	CreatedAt string `json:"created_at"`
	VCPUNum   int64  `json:"vcpu_num"`
	RamMB     int64  `json:"ram_mb"`
}

type EventData struct {
	KillReason string    `json:"kill_reason,omitempty"`
	Execution  Execution `json:"execution"`
}

type Event struct {
	EventID   string    `json:"event_id"`
	Version   string    `json:"version"`
	Type      string    `json:"type"`
	Timestamp string    `json:"timestamp"`
	SandboxID string    `json:"sandbox_id"`
	EventData EventData `json:"event_data"`
}

// Dispatcher stores webhook registrations only in memory and dispatches events asynchronously.
type Dispatcher struct {
	mu       sync.RWMutex
	webhooks map[string]runtimeapi.WebhookRecord
	client   *http.Client
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		webhooks: make(map[string]runtimeapi.WebhookRecord),
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (d *Dispatcher) Create(opts runtimeapi.WebhookCreateOptions) (runtimeapi.WebhookRecord, error) {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return runtimeapi.WebhookRecord{}, ErrInvalidArgument.Wrap(fmt.Errorf("name is required"))
	}
	parsedURL, err := url.ParseRequestURI(strings.TrimSpace(opts.URL))
	if err != nil || parsedURL.Host == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return runtimeapi.WebhookRecord{}, ErrInvalidArgument.Wrap(fmt.Errorf("url must be a valid HTTP or HTTPS URL"))
	}
	events, err := normalizeEvents(opts.Events)
	if err != nil {
		return runtimeapi.WebhookRecord{}, ErrInvalidArgument.Wrap(err)
	}
	webhookID, err := id.NewWithPrefix("wh_")
	if err != nil {
		return runtimeapi.WebhookRecord{}, err
	}
	hook := runtimeapi.WebhookRecord{WebhookID: webhookID, Name: name, URL: parsedURL.String(), Events: events, CreatedAt: time.Now().UTC()}
	d.mu.Lock()
	d.webhooks[hook.WebhookID] = hook
	d.mu.Unlock()
	return hook, nil
}

func (d *Dispatcher) List() []runtimeapi.WebhookRecord {
	if d == nil {
		return []runtimeapi.WebhookRecord{}
	}
	d.mu.RLock()
	hooks := make([]runtimeapi.WebhookRecord, 0, len(d.webhooks))
	for _, hook := range d.webhooks {
		hook.Events = append([]string(nil), hook.Events...)
		hooks = append(hooks, hook)
	}
	d.mu.RUnlock()
	sort.Slice(hooks, func(i, j int) bool { return hooks[i].CreatedAt.Before(hooks[j].CreatedAt) })
	return hooks
}

func (d *Dispatcher) Delete(webhookID string) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	_, found := d.webhooks[webhookID]
	delete(d.webhooks, webhookID)
	d.mu.Unlock()
	return found
}

func (d *Dispatcher) Publish(event Event) {
	if d == nil || !isSupportedEvent(event.Type) {
		return
	}
	d.mu.RLock()
	for _, hook := range d.webhooks {
		if subscribesTo(hook, event.Type) {
			go d.deliver(hook, event)
		}
	}
	d.mu.RUnlock()
}

func (d *Dispatcher) deliver(hook runtimeapi.WebhookRecord, event Event) {
	body, err := json.Marshal(event)
	if err != nil {
		ulog.GetLogger().Error("failed to marshal webhook event", ulog.F("event_id", event.EventID), ulog.F("webhook_id", hook.WebhookID), ulog.F("error", err))
		return
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequest(http.MethodPost, hook.URL, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("conch-webhook-id", hook.WebhookID)
			resp, doErr := d.client.Do(req)
			if doErr == nil {
				resp.Body.Close()
				if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
					return
				}
				lastErr = fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
			} else {
				lastErr = doErr
			}
		} else {
			lastErr = err
		}
	}
	ulog.GetLogger().Error("webhook delivery failed after retries", ulog.F("event_id", event.EventID), ulog.F("webhook_id", hook.WebhookID), ulog.F("url", hook.URL), ulog.F("error", lastErr))
}

func NewEvent(eventType, sandboxID, killReason string, execution Execution) (Event, error) {
	eventID, err := id.NewWithPrefix("evt_")
	if err != nil {
		return Event{}, err
	}
	return Event{EventID: eventID, Version: "v1", Type: eventType, Timestamp: time.Now().UTC().Format(time.RFC3339), SandboxID: sandboxID, EventData: EventData{KillReason: killReason, Execution: execution}}, nil
}

func normalizeEvents(events []string) ([]string, error) {
	if len(events) == 0 {
		return []string{EventSandboxCreated, EventSandboxKilled}, nil
	}
	seen := make(map[string]struct{}, len(events))
	result := make([]string, 0, len(events))
	for _, event := range events {
		event = strings.TrimSpace(event)
		if !isSupportedEvent(event) {
			return nil, fmt.Errorf("unsupported event %q", event)
		}
		if _, exists := seen[event]; !exists {
			seen[event] = struct{}{}
			result = append(result, event)
		}
	}
	return result, nil
}

func subscribesTo(hook runtimeapi.WebhookRecord, eventType string) bool {
	for _, event := range hook.Events {
		if event == eventType {
			return true
		}
	}
	return false
}

func isSupportedEvent(event string) bool { _, ok := supportedEvents[event]; return ok }
