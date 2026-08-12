package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/openeuler/Conch/internal/apperror"
	"github.com/openeuler/Conch/pkg/ulog"
)

type apiErrorResponse struct {
	Status string        `json:"status"`
	Code   apperror.Code `json:"code"`
	Error  string        `json:"error"`
}

var (
	errRequestInvalidBody = apperror.Define(
		apperror.InvalidArgument,
		"request.invalid_body",
		"invalid request body",
	)
	errRequestInvalidMultipart = apperror.Define(
		apperror.InvalidArgument,
		"request.invalid_multipart",
		"invalid multipart request",
	)
	errServiceUnavailable = apperror.Define(
		apperror.Unavailable,
		"service.unavailable",
		"service unavailable",
	)
	errRequestDeadlineExceeded = apperror.Define(
		apperror.DeadlineExceeded,
		"request.deadline_exceeded",
		"request deadline exceeded",
	)
)

var internalAPIError = apiErrorResponse{
	Status: "error",
	Code:   "internal",
	Error:  "internal server error",
}

func classifyAPIError(err error) (int, apiErrorResponse) {
	var appErr *apperror.Error
	if errors.As(err, &appErr) {
		code := appErr.Code()
		message := strings.TrimSpace(appErr.PublicMessage())
		kind := appErr.Kind()
		if code.Valid() && message != "" && kind >= apperror.Internal && kind <= apperror.NotImplemented {
			return httpStatusForErrorKind(kind), apiErrorResponse{
				Status: "error",
				Code:   code,
				Error:  message,
			}
		}
		return http.StatusInternalServerError, internalAPIError
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, apiErrorResponse{
			Status: "error",
			Code:   errRequestDeadlineExceeded.Code(),
			Error:  errRequestDeadlineExceeded.PublicMessage(),
		}
	}
	return http.StatusInternalServerError, internalAPIError
}

func httpStatusForErrorKind(kind apperror.Kind) int {
	switch kind {
	case apperror.InvalidArgument:
		return http.StatusBadRequest
	case apperror.Unauthenticated:
		return http.StatusUnauthorized
	case apperror.PermissionDenied:
		return http.StatusForbidden
	case apperror.NotFound:
		return http.StatusNotFound
	case apperror.AlreadyExists, apperror.Conflict, apperror.FailedPrecondition:
		return http.StatusConflict
	case apperror.ResourceExhausted:
		return http.StatusTooManyRequests
	case apperror.UpstreamFailure:
		return http.StatusBadGateway
	case apperror.Unavailable:
		return http.StatusServiceUnavailable
	case apperror.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case apperror.NotImplemented:
		return http.StatusNotImplemented
	case apperror.Internal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

func writeErrorResponse(w http.ResponseWriter, status int, response apiErrorResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func writeAPIError(w http.ResponseWriter, err error, fields ...ulog.Field) {
	status, response := classifyAPIError(err)
	if status >= http.StatusInternalServerError {
		fields = append([]ulog.Field{
			ulog.F("status_code", status),
			ulog.F("error_code", response.Code),
			ulog.F("error", err),
		}, fields...)
		ulog.GetLogger().Error("API request failed", fields...)
	}
	writeErrorResponse(w, status, response)
}

func writeMethodNotAllowed(w http.ResponseWriter) {
	writeErrorResponse(w, http.StatusMethodNotAllowed, apiErrorResponse{
		Status: "error",
		Code:   "request.method_not_allowed",
		Error:  "method not allowed",
	})
}
