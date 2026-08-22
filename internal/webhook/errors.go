package webhook

import "github.com/openeuler/Conch/internal/apperror"

var (
	ErrInvalidArgument = apperror.Define(apperror.InvalidArgument, "webhook.invalid_argument", "invalid webhook argument")
	ErrNotFound        = apperror.Define(apperror.NotFound, "webhook.not_found", "webhook not found")
)
