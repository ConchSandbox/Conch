// Package buildahcli resolves the buildah executable used by conch (subprocess mode).
package buildahcli

import "os"

// Bin returns the buildah executable path: CONCH_BUILDAH_BIN if set, otherwise "buildah".
func Bin() string {
	if b := os.Getenv("CONCH_BUILDAH_BIN"); b != "" {
		return b
	}
	return "buildah"
}
