package ocipublisher

import (
	"errors"

	"go.podman.io/storage"
)

// claimImageNames removes names from any existing image records so the caller
// can safely reuse stable tags across repeated local builds.
func claimImageNames(store storage.Store, names []string, keepImageID string) error {
	for _, name := range names {
		if name == "" {
			continue
		}

		img, err := store.Image(name)
		if err != nil {
			if errors.Is(err, storage.ErrImageUnknown) {
				continue
			}
			return err
		}
		if img == nil || img.ID == keepImageID {
			continue
		}
		if err := store.RemoveNames(img.ID, []string{name}); err != nil {
			return err
		}
	}
	return nil
}
