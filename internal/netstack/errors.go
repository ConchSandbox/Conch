package netstack

import "github.com/openeuler/Conch/internal/apperror"

var ErrInvalidPolicy = apperror.Define(
	apperror.InvalidArgument,
	"network.invalid_policy",
	"invalid sandbox network policy",
)
