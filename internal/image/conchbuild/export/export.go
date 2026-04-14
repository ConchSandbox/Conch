package export

import (
	"context"
	"fmt"
	"io"

	"go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
)

// ExportImageToTar exports an image from containers-storage to a tar archive file.
// Format can be "oci-archive" or "docker-archive". Tag is used for the archive reference.
func ExportImageToTar(ctx context.Context, imageRef, destPath, format, tag string, systemContext *types.SystemContext, reportWriter io.Writer) error {
	srcRef, err := alltransports.ParseImageName("containers-storage:" + imageRef)
	if err != nil {
		return fmt.Errorf("parse source ref: %w", err)
	}
	destRef, err := alltransports.ParseImageName(format + ":" + destPath + ":" + tag)
	if err != nil {
		return fmt.Errorf("parse dest ref: %w", err)
	}

	policy, err := signature.NewPolicyContext(&signature.Policy{
		Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()},
	})
	if err != nil {
		return err
	}
	defer policy.Destroy()

	opts := &copy.Options{
		SourceCtx:      systemContext,
		DestinationCtx: systemContext,
	}
	if reportWriter != nil {
		opts.ReportWriter = reportWriter
	}

	_, err = copy.Image(ctx, policy, destRef, srcRef, opts)
	if err != nil {
		return fmt.Errorf("export image: %w", err)
	}
	return nil
}
