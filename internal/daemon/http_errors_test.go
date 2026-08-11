package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openeuler/Conch/internal/apperror"
	"github.com/openeuler/Conch/pkg/ulog"
)

type recordingAPILogger struct {
	messages []string
	fields   [][]ulog.Field
}

func (l *recordingAPILogger) Debug(string, ...ulog.Field) {}
func (l *recordingAPILogger) Info(string, ...ulog.Field)  {}
func (l *recordingAPILogger) Warn(string, ...ulog.Field)  {}
func (l *recordingAPILogger) Fatal(string, ...ulog.Field) {}
func (l *recordingAPILogger) Error(message string, fields ...ulog.Field) {
	l.messages = append(l.messages, message)
	l.fields = append(l.fields, append([]ulog.Field(nil), fields...))
}
func (l *recordingAPILogger) With(...ulog.Field) ulog.Logger               { return l }
func (l *recordingAPILogger) WithContext(context.Context) ulog.Logger      { return l }
func (l *recordingAPILogger) ReplaceField(string, interface{}) ulog.Logger { return l }

func TestHTTPStatusForErrorKind(t *testing.T) {
	tests := []struct {
		kind apperror.Kind
		want int
	}{
		{apperror.Internal, http.StatusInternalServerError},
		{apperror.InvalidArgument, http.StatusBadRequest},
		{apperror.Unauthenticated, http.StatusUnauthorized},
		{apperror.PermissionDenied, http.StatusForbidden},
		{apperror.NotFound, http.StatusNotFound},
		{apperror.AlreadyExists, http.StatusConflict},
		{apperror.Conflict, http.StatusConflict},
		{apperror.FailedPrecondition, http.StatusConflict},
		{apperror.ResourceExhausted, http.StatusTooManyRequests},
		{apperror.UpstreamFailure, http.StatusBadGateway},
		{apperror.Unavailable, http.StatusServiceUnavailable},
		{apperror.DeadlineExceeded, http.StatusGatewayTimeout},
		{apperror.NotImplemented, http.StatusNotImplemented},
		{apperror.Kind(255), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		if got := httpStatusForErrorKind(tt.kind); got != tt.want {
			t.Errorf("httpStatusForErrorKind(%d) = %d, want %d", tt.kind, got, tt.want)
		}
	}
}

func TestWriteAPIErrorDoesNotExposeCause(t *testing.T) {
	prototype := apperror.Define(apperror.InvalidArgument, "test.invalid_argument", "safe public message")
	recorder := httptest.NewRecorder()
	writeAPIError(recorder, prototype.Wrap(errors.New("secret internal path /srv/private")))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	want := "{\"status\":\"error\",\"code\":\"test.invalid_argument\",\"error\":\"safe public message\"}\n"
	if recorder.Body.String() != want {
		t.Fatalf("body = %q, want %q", recorder.Body.String(), want)
	}
}

func TestWriteAPIErrorUsesSafeFallback(t *testing.T) {
	previousLogger := ulog.GetLogger()
	logger := &recordingAPILogger{}
	ulog.SetLogger(logger)
	t.Cleanup(func() { ulog.SetLogger(previousLogger) })

	cause := errors.New("database password and private path")
	recorder := httptest.NewRecorder()
	writeAPIError(recorder, cause, ulog.F("sandbox_id", "sandbox-1"))
	if recorder.Code != http.StatusInternalServerError || recorder.Body.String() != "{\"status\":\"error\",\"code\":\"internal\",\"error\":\"internal server error\"}\n" {
		t.Fatalf("response = status:%d body:%q", recorder.Code, recorder.Body.String())
	}
	if len(logger.messages) != 1 || logger.messages[0] != "API request failed" {
		t.Fatalf("error logs = %#v, want one API failure log", logger.messages)
	}
	if got := fieldValue(logger.fields[0], "error"); got != cause {
		t.Fatalf("logged error = %v, want original cause", got)
	}
	if got := fieldValue(logger.fields[0], "sandbox_id"); got != "sandbox-1" {
		t.Fatalf("logged sandbox_id = %v, want sandbox-1", got)
	}
}

func fieldValue(fields []ulog.Field, key string) interface{} {
	for _, field := range fields {
		if field.Key == key {
			return field.Value
		}
	}
	return nil
}

func TestClassifyAPIErrorUsesOutermostApplicationError(t *testing.T) {
	inner := apperror.Define(apperror.NotFound, "inner.not_found", "inner not found")
	outer := apperror.Define(apperror.Conflict, "outer.conflict", "outer conflict")
	status, response := classifyAPIError(outer.Wrap(inner.New()))
	if status != http.StatusConflict || response.Code != "outer.conflict" || response.Error != "outer conflict" {
		t.Fatalf("classification = status:%d response:%#v", status, response)
	}
}

func TestClassifyAPIErrorRejectsIncompleteApplicationError(t *testing.T) {
	status, response := classifyAPIError(&apperror.Error{})
	if status != http.StatusInternalServerError || response != internalAPIError {
		t.Fatalf("classification = status:%d response:%#v", status, response)
	}
}
