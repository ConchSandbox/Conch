package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openeuler/Conch/internal/webhook"
)

func TestWebhookManagementHandlers(t *testing.T) {
	server := &Daemon{router: http.NewServeMux(), webhookDispatcher: webhook.NewDispatcher()}
	server.routes()
	create := httptest.NewRecorder()
	server.router.ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/v1/events/webhooks", bytes.NewBufferString(`{"name":"reliability","url":"https://example.test/events","events":["sandbox.lifecycle.killed"]}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", create.Code, create.Body.String())
	}
	var hook webhookResponse
	if err := json.NewDecoder(create.Body).Decode(&hook); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	list := httptest.NewRecorder()
	server.router.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/events/webhooks", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d", list.Code)
	}
	var response listWebhooksResponse
	if err := json.NewDecoder(list.Body).Decode(&response); err != nil || len(response.Webhooks) != 1 {
		t.Fatalf("list = %#v, err = %v", response, err)
	}
	deleteResponse := httptest.NewRecorder()
	server.router.ServeHTTP(deleteResponse, httptest.NewRequest(http.MethodDelete, "/api/v1/events/webhooks/"+hook.WebhookID, nil))
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete status = %d", deleteResponse.Code)
	}
	missing := httptest.NewRecorder()
	server.router.ServeHTTP(missing, httptest.NewRequest(http.MethodDelete, "/api/v1/events/webhooks/"+hook.WebhookID, nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing delete status = %d", missing.Code)
	}
	var missingError apiErrorResponse
	if err := json.NewDecoder(missing.Body).Decode(&missingError); err != nil || missingError.Code != "webhook.not_found" {
		t.Fatalf("missing delete error = %#v, err = %v", missingError, err)
	}
	invalid := httptest.NewRecorder()
	server.router.ServeHTTP(invalid, httptest.NewRequest(http.MethodPost, "/api/v1/events/webhooks", bytes.NewBufferString(`{"name":"","url":"https://example.test"}`)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid create status = %d", invalid.Code)
	}
	var invalidError apiErrorResponse
	if err := json.NewDecoder(invalid.Body).Decode(&invalidError); err != nil || invalidError.Code != "webhook.invalid_argument" {
		t.Fatalf("invalid create error = %#v, err = %v", invalidError, err)
	}
}
