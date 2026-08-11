package volume

import "github.com/openeuler/Conch/internal/apperror"

var ErrInvalidMount = apperror.Define(
	apperror.InvalidArgument,
	"volume.invalid_mount",
	"invalid volume mount",
)
