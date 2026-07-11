package conchruntime

import (
	"context"
	"errors"
	"io"
	"testing"

	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/image/erofsconvert"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

type templateBuildImageOps struct {
	prepareResult conchimage.PrepareRootfsSourceResult
	convertResult erofsconvert.ConvertRootfsResult
	publishResult conchimage.PublishBootImageResult
	publishErr    error
	removeErr     error
	removeCalls   []runtimeapi.RemoveImageOptions
}

func (f *templateBuildImageOps) Pull(context.Context, runtimeapi.PullImageOptions) (runtimeapi.PullImageResult, error) {
	return runtimeapi.PullImageResult{}, nil
}

func (f *templateBuildImageOps) Push(context.Context, runtimeapi.PushImageOptions) error {
	return nil
}

func (f *templateBuildImageOps) List(context.Context, runtimeapi.ListImagesOptions) ([]runtimeapi.ImageRecord, error) {
	return nil, nil
}

func (f *templateBuildImageOps) Remove(_ context.Context, opts runtimeapi.RemoveImageOptions) error {
	f.removeCalls = append(f.removeCalls, opts)
	return f.removeErr
}

func (f *templateBuildImageOps) Unpack(context.Context, runtimeapi.UnpackImageOptions) (map[string]string, error) {
	return nil, nil
}

func (f *templateBuildImageOps) ImportArchive(context.Context, io.Reader, runtimeapi.ImportImageArchiveOptions) (runtimeapi.ImportImageArchiveResult, error) {
	return runtimeapi.ImportImageArchiveResult{}, nil
}

func (f *templateBuildImageOps) ExportArchive(context.Context, io.Writer, runtimeapi.ExportImageArchiveOptions) error {
	return nil
}

func (f *templateBuildImageOps) PrepareRootfsSource(context.Context, conchimage.PrepareRootfsSourceOptions) (conchimage.PrepareRootfsSourceResult, error) {
	return f.prepareResult, nil
}

func (f *templateBuildImageOps) PublishBootImage(context.Context, conchimage.PublishBootImageOptions) (conchimage.PublishBootImageResult, error) {
	return f.publishResult, f.publishErr
}

func (f *templateBuildImageOps) ConvertRootfsToErofs(context.Context, erofsconvert.ConvertRootfsRequest) (erofsconvert.ConvertRootfsResult, error) {
	return f.convertResult, nil
}

func TestCreateTemplateFromSourceRemovesTemporaryConvertedImage(t *testing.T) {
	imageOps := &templateBuildImageOps{
		prepareResult: conchimage.PrepareRootfsSourceResult{ImageName: "localhost:5000/busybox:latest"},
		convertResult: erofsconvert.ConvertRootfsResult{ImageName: "conch-erofs-rootfs:tmpl-1"},
		publishResult: conchimage.PublishBootImageResult{
			BootIndexDigest: "sha256:index",
			RootfsKey:       "rootfs-key",
			VMKey:           "vm-key",
			ImageName:       "localhost/conch/busybox:latest",
		},
	}
	svc := &Service{Image: imageOps}

	result, err := svc.createTemplateFromSource(context.Background(), "team-a", "tmpl-1", TemplateCreateOptions{
		Source:       "localhost:5000/busybox:latest",
		KernelPath:   "/kernel",
		InitrdPath:   "/initrd",
		BootIndexTag: "localhost/conch/busybox:latest",
	})
	if err != nil {
		t.Fatalf("createTemplateFromSource() error = %v", err)
	}
	if result.bootIndexTag != "localhost/conch/busybox:latest" {
		t.Fatalf("boot index tag = %q", result.bootIndexTag)
	}
	if len(imageOps.removeCalls) != 1 {
		t.Fatalf("remove calls = %#v, want one", imageOps.removeCalls)
	}
	remove := imageOps.removeCalls[0]
	if remove.Namespace != "team-a" || remove.ImageName != "conch-erofs-rootfs:tmpl-1" || remove.Synchronous {
		t.Fatalf("remove options = %#v", remove)
	}
}

func TestCreateTemplateFromSourceIgnoresTemporaryImageCleanupFailure(t *testing.T) {
	imageOps := &templateBuildImageOps{
		prepareResult: conchimage.PrepareRootfsSourceResult{ImageName: "source"},
		convertResult: erofsconvert.ConvertRootfsResult{ImageName: "conch-erofs-rootfs:tmpl-1"},
		publishResult: conchimage.PublishBootImageResult{
			RootfsKey: "rootfs-key",
			VMKey:     "vm-key",
			ImageName: "boot-index",
		},
		removeErr: errors.New("remove failed"),
	}
	svc := &Service{Image: imageOps}

	if _, err := svc.createTemplateFromSource(context.Background(), "default", "tmpl-1", TemplateCreateOptions{
		Source: "source", KernelPath: "/kernel", InitrdPath: "/initrd",
	}); err != nil {
		t.Fatalf("cleanup failure must not fail template creation: %v", err)
	}
	if len(imageOps.removeCalls) != 1 {
		t.Fatalf("remove calls = %d, want one", len(imageOps.removeCalls))
	}
}

func TestCreateTemplateFromSourceKeepsTemporaryImageWhenPublishFails(t *testing.T) {
	imageOps := &templateBuildImageOps{
		prepareResult: conchimage.PrepareRootfsSourceResult{ImageName: "source"},
		convertResult: erofsconvert.ConvertRootfsResult{ImageName: "conch-erofs-rootfs:tmpl-1"},
		publishErr:    errors.New("publish failed"),
	}
	svc := &Service{Image: imageOps}

	if _, err := svc.createTemplateFromSource(context.Background(), "default", "tmpl-1", TemplateCreateOptions{
		Source: "source", KernelPath: "/kernel", InitrdPath: "/initrd",
	}); err == nil {
		t.Fatal("createTemplateFromSource() error = nil, want publish failure")
	}
	if len(imageOps.removeCalls) != 0 {
		t.Fatalf("remove calls = %#v, want none before a successful publish", imageOps.removeCalls)
	}
}
