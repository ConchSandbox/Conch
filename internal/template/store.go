package template

import (
	"context"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type Filter struct {
	Origin   Origin
	BootMode BootMode
}

type Store interface {
	Put(context.Context, Entry, ocispec.Descriptor) (Entry, error)
	Get(context.Context, string) (Entry, error)
	List(context.Context, Filter) ([]Entry, error)
	Delete(context.Context, string) error
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
