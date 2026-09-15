package volume

import "github.com/openeuler/Conch/internal/apperror"

var (
	ErrInvalidMount = apperror.Define(
		apperror.InvalidArgument,
		"volume.invalid_mount",
		"invalid volume mount",
	)
	ErrNotFound      = apperror.Define(apperror.NotFound, "volume.not_found", "volume not found")
	ErrAlreadyExists = apperror.Define(apperror.AlreadyExists, "volume.already_exists", "volume already exists")
	ErrInUse         = apperror.Define(apperror.Conflict, "volume.in_use", "volume is mounted by a sandbox")
)
