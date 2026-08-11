package sandbox

import "github.com/openeuler/Conch/internal/apperror"

var (
	ErrInvalidArgument        = apperror.Define(apperror.InvalidArgument, "sandbox.invalid_argument", "invalid sandbox argument")
	ErrInvalidEnvironment     = apperror.Define(apperror.InvalidArgument, "sandbox.invalid_environment", "invalid sandbox environment")
	ErrInitializationTooLarge = apperror.Define(apperror.InvalidArgument, "sandbox.initialization_too_large", "sandbox initialization message is too large")
	ErrNotFound               = apperror.Define(apperror.NotFound, "sandbox.not_found", "sandbox not found")
	ErrAlreadyExists          = apperror.Define(apperror.AlreadyExists, "sandbox.already_exists", "sandbox already exists")
	ErrFailedPrecondition     = apperror.Define(apperror.FailedPrecondition, "sandbox.failed_precondition", "sandbox operation failed its precondition")
	ErrResourceExhausted      = apperror.Define(apperror.ResourceExhausted, "sandbox.resource_exhausted", "sandbox resources are exhausted")
)
