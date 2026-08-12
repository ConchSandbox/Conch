package template

import "github.com/openeuler/Conch/internal/apperror"

var (
	ErrInvalidArgument    = apperror.Define(apperror.InvalidArgument, "template.invalid_argument", "invalid template argument")
	ErrInvalidArtifact    = apperror.Define(apperror.InvalidArgument, "template.invalid_artifact", "invalid template artifact")
	ErrNotFound           = apperror.Define(apperror.NotFound, "template.not_found", "template not found")
	ErrAlreadyExists      = apperror.Define(apperror.AlreadyExists, "template.already_exists", "template already exists")
	ErrFailedPrecondition = apperror.Define(apperror.FailedPrecondition, "template.failed_precondition", "template operation failed its precondition")
)
