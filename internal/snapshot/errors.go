package snapshot

import "github.com/openeuler/Conch/internal/apperror"

var (
	ErrInvalidArgument    = apperror.Define(apperror.InvalidArgument, "snapshot.invalid_argument", "invalid snapshot argument")
	ErrNotFound           = apperror.Define(apperror.NotFound, "snapshot.not_found", "snapshot not found")
	ErrFailedPrecondition = apperror.Define(apperror.FailedPrecondition, "snapshot.failed_precondition", "snapshot operation failed its precondition")
)
