package conchruntime

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/daemon/state"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/image/erofsconvert"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

type bootIndexCall struct {
	Namespace       string
	BootIndexDigest string
}

type bootIndexReferenceCall struct {
	Namespace string
	Reference string
}

type templateBuildImageOps struct {
	pullErr            error
	pullCalls          []runtimeapi.PullImageOptions
	pushErr            error
	pushCalls          []runtimeapi.PushImageOptions
	pushBootIndexErr   error
	pushBootIndexCalls []conchimage.PushBootIndexOptions

	prepareResult conchimage.PrepareRootfsSourceResult
	convertResult erofsconvert.ConvertRootfsResult
	publishResult conchimage.PublishBootImageResult
	publishErr    error
	inspectResult conchimage.BootIndexInfo
	inspectErr    error
	inspectCalls  []bootIndexCall

	inspectReferenceResult conchimage.BootIndexInfo
	inspectReferenceErr    error
	inspectReferenceCalls  []bootIndexReferenceCall

	checkpointResults []conchimage.PublishCheckpointBootImageResult
	checkpointErr     error
	checkpointCalls   []conchimage.PublishCheckpointBootImageOptions

	removeErr   error
	removeCalls []runtimeapi.RemoveImageOptions
	unpackCalls []runtimeapi.UnpackImageOptions
}

func (f *templateBuildImageOps) Pull(_ context.Context, opts runtimeapi.PullImageOptions) (runtimeapi.PullImageResult, error) {
	f.pullCalls = append(f.pullCalls, opts)
	return runtimeapi.PullImageResult{}, f.pullErr
}

func (f *templateBuildImageOps) Push(_ context.Context, opts runtimeapi.PushImageOptions) error {
	f.pushCalls = append(f.pushCalls, opts)
	return f.pushErr
}

func (f *templateBuildImageOps) PushBootIndex(_ context.Context, opts conchimage.PushBootIndexOptions) error {
	f.pushBootIndexCalls = append(f.pushBootIndexCalls, opts)
	return f.pushBootIndexErr
}

func (f *templateBuildImageOps) List(context.Context, runtimeapi.ListImagesOptions) ([]runtimeapi.ImageRecord, error) {
	return nil, nil
}

func (f *templateBuildImageOps) Remove(_ context.Context, opts runtimeapi.RemoveImageOptions) error {
	f.removeCalls = append(f.removeCalls, opts)
	return f.removeErr
}

func (f *templateBuildImageOps) Unpack(_ context.Context, opts runtimeapi.UnpackImageOptions) (map[string]string, error) {
	f.unpackCalls = append(f.unpackCalls, opts)
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

func (f *templateBuildImageOps) InspectBootIndex(_ context.Context, namespace, bootIndexDigest string) (conchimage.BootIndexInfo, error) {
	f.inspectCalls = append(f.inspectCalls, bootIndexCall{Namespace: namespace, BootIndexDigest: bootIndexDigest})
	result := f.inspectResult
	if result.BootIndexDigest == "" {
		result.BootIndexDigest = bootIndexDigest
	}
	return result, f.inspectErr
}

func (f *templateBuildImageOps) InspectBootIndexReference(_ context.Context, namespace, reference string) (conchimage.BootIndexInfo, error) {
	f.inspectReferenceCalls = append(f.inspectReferenceCalls, bootIndexReferenceCall{Namespace: namespace, Reference: reference})
	return f.inspectReferenceResult, f.inspectReferenceErr
}

func (f *templateBuildImageOps) PublishCheckpointBootImage(_ context.Context, opts conchimage.PublishCheckpointBootImageOptions) (conchimage.PublishCheckpointBootImageResult, error) {
	call := len(f.checkpointCalls)
	f.checkpointCalls = append(f.checkpointCalls, opts)
	if f.checkpointErr != nil {
		return conchimage.PublishCheckpointBootImageResult{}, f.checkpointErr
	}
	if call < len(f.checkpointResults) {
		return f.checkpointResults[call], nil
	}
	return conchimage.PublishCheckpointBootImageResult{}, nil
}

func (f *templateBuildImageOps) ConvertRootfsToErofs(context.Context, erofsconvert.ConvertRootfsRequest) (erofsconvert.ConvertRootfsResult, error) {
	return f.convertResult, nil
}

func TestPullTemplateCreatesReadyTemplateAfterStaticValidation(t *testing.T) {
	for _, tt := range []struct {
		name       string
		resume     bool
		wantOrigin string
		wantMode   string
	}{
		{name: "cold image", wantOrigin: state.TemplateOriginImage, wantMode: state.TemplateBootModeCold},
		{name: "resume checkpoint", resume: true, wantOrigin: state.TemplateOriginCheckpoint, wantMode: state.TemplateBootModeResume},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			bootIndexDigest := digest.FromString("pulled-" + tt.name).String()
			const reference = "registry.example.invalid/conch/template:latest"
			imageOps := &templateBuildImageOps{inspectReferenceResult: conchimage.BootIndexInfo{
				BootIndexDigest: bootIndexDigest,
				Resume:          tt.resume,
				VMMName:         "cloud-hypervisor",
			}}
			store := newTestStore(t)
			svc := New(nil, imageOps, imageOps, store, "default")

			result, err := svc.PullTemplate(ctx, TemplatePullOptions{
				Reference: reference,
				Namespace: "team-a",
				PlainHTTP: true,
				Username:  "registry-user",
				Password:  "registry-pass",
				Labels:    map[string]string{"source": "registry"},
			})
			if err != nil {
				t.Fatalf("PullTemplate() error = %v", err)
			}
			if result.TemplateID == "" || result.BootIndexDigest != bootIndexDigest || result.BuildRef != reference {
				t.Fatalf("PullTemplate() = %#v", result)
			}
			if len(imageOps.pullCalls) != 1 {
				t.Fatalf("Pull() calls = %#v", imageOps.pullCalls)
			}
			pull := imageOps.pullCalls[0]
			if pull.ImageName != reference || pull.Namespace != "team-a" || !pull.SkipUnpack ||
				!pull.PlainHTTP || pull.Username != "registry-user" || pull.Password != "registry-pass" {
				t.Fatalf("Pull() request = %#v", pull)
			}
			if len(imageOps.unpackCalls) != 0 {
				t.Fatalf("PullTemplate unpacked content during static validation: %#v", imageOps.unpackCalls)
			}
			if len(imageOps.inspectReferenceCalls) != 1 || imageOps.inspectReferenceCalls[0] != (bootIndexReferenceCall{
				Namespace: "team-a",
				Reference: reference,
			}) {
				t.Fatalf("InspectBootIndexReference() calls = %#v", imageOps.inspectReferenceCalls)
			}

			rec, err := store.GetTemplate(ctx, result.TemplateID)
			if err != nil {
				t.Fatalf("GetTemplate() error = %v", err)
			}
			if rec.State != state.TemplateReady ||
				rec.BootIndexDigest != bootIndexDigest || rec.Origin != tt.wantOrigin || rec.BootMode != tt.wantMode {
				t.Fatalf("pulled Template = %#v", rec)
			}
			if rec.BuildRef != reference || rec.ImageName != reference || rec.Labels["source"] != "registry" {
				t.Fatalf("pulled Template metadata = %#v", rec)
			}
		})
	}
}

func TestPushTemplateUsesImmutableBootIndexDigest(t *testing.T) {
	ctx := context.Background()
	bootIndexDigest := digest.FromString("template-to-push").String()
	const buildRef = "registry.example.invalid/conch/template:source"
	imageOps := &templateBuildImageOps{inspectReferenceResult: conchimage.BootIndexInfo{
		BootIndexDigest: bootIndexDigest,
	}}
	store := newTestStore(t)
	svc := New(&fakeSandboxOps{}, imageOps, imageOps, store, "default")
	pulled, err := svc.PullTemplate(ctx, TemplatePullOptions{Reference: buildRef, Namespace: "team-a"})
	if err != nil {
		t.Fatalf("PullTemplate() error = %v", err)
	}
	// A registry/local image name is mutable. Retargeting the source reference
	// after Template creation must not change what this Template pushes.
	imageOps.inspectReferenceResult.BootIndexDigest = digest.FromString("retargeted-source-reference").String()

	err = svc.PushTemplate(ctx, TemplatePushOptions{
		TemplateID:      pulled.TemplateID,
		RemoteReference: "mirror.example.invalid/conch/template:copy",
		Namespace:       "team-a",
		PlainHTTP:       true,
		Username:        "push-user",
		Password:        "push-pass",
		RegistryTimeout: "5m",
	})
	if err != nil {
		t.Fatalf("PushTemplate() error = %v", err)
	}
	if len(imageOps.pushCalls) != 0 {
		t.Fatalf("PushTemplate used mutable image-name push path: %#v", imageOps.pushCalls)
	}
	if len(imageOps.pushBootIndexCalls) != 1 {
		t.Fatalf("PushBootIndex() calls = %#v", imageOps.pushBootIndexCalls)
	}
	if got, want := imageOps.pushBootIndexCalls[0], (conchimage.PushBootIndexOptions{
		BootIndexDigest: bootIndexDigest,
		RemoteReference: "mirror.example.invalid/conch/template:copy",
		Namespace:       "team-a",
		PlainHTTP:       true,
		Username:        "push-user",
		Password:        "push-pass",
		RegistryTimeout: "5m",
	}); got != want {
		t.Fatalf("PushBootIndex() request = %#v, want %#v", got, want)
	}
}

func TestCreateTemplateStoresStaticallyValidatedBootIndexReadyState(t *testing.T) {
	ctx := context.Background()
	bootIndexDigest := digest.FromString("cold-template-boot-index").String()
	imageOps := &templateBuildImageOps{
		prepareResult: conchimage.PrepareRootfsSourceResult{ImageName: "localhost:5000/busybox:latest"},
		convertResult: erofsconvert.ConvertRootfsResult{ImageName: "conch-erofs-rootfs:cold-template"},
		publishResult: conchimage.PublishBootImageResult{
			BootIndexDigest: bootIndexDigest,
			ImageName:       "localhost/conch/templates:cold",
		},
		inspectResult: conchimage.BootIndexInfo{
			BootIndexDigest: bootIndexDigest,
			Resume:          false,
		},
	}
	store := newTestStore(t)
	svc := New(nil, imageOps, imageOps, store, "default")

	result, err := svc.CreateTemplate(ctx, TemplateCreateOptions{
		Namespace:    "team-a",
		Source:       "localhost:5000/busybox:latest",
		KernelPath:   "/kernel",
		InitrdPath:   "/initrd",
		BootIndexTag: "localhost/conch/templates:cold",
	})
	if err != nil {
		t.Fatalf("CreateTemplate() error = %v", err)
	}
	if result.TemplateID == "" || result.BootIndexDigest != bootIndexDigest || result.BootIndexTag != "localhost/conch/templates:cold" {
		t.Fatalf("CreateTemplate() = %#v", result)
	}
	if len(imageOps.inspectCalls) != 1 || imageOps.inspectCalls[0] != (bootIndexCall{
		Namespace:       "team-a",
		BootIndexDigest: bootIndexDigest,
	}) {
		t.Fatalf("InspectBootIndex() calls = %#v", imageOps.inspectCalls)
	}

	rec, err := store.GetTemplate(ctx, result.TemplateID)
	if err != nil {
		t.Fatalf("GetTemplate() error = %v", err)
	}
	if rec.State != state.TemplateReady ||
		rec.BootIndexDigest != bootIndexDigest || rec.BootMode != state.TemplateBootModeCold {
		t.Fatalf("template READY state = %#v", rec)
	}
	if rec.BuildRef != "localhost/conch/templates:cold" {
		t.Fatalf("template BuildRef = %q", rec.BuildRef)
	}
}

func TestCreateTemplateFromSourceRemovesTemporaryConvertedImage(t *testing.T) {
	imageOps := &templateBuildImageOps{
		prepareResult: conchimage.PrepareRootfsSourceResult{ImageName: "localhost:5000/busybox:latest"},
		convertResult: erofsconvert.ConvertRootfsResult{ImageName: "conch-erofs-rootfs:tmpl-1"},
		publishResult: conchimage.PublishBootImageResult{
			BootIndexDigest: "sha256:index",
			ImageName:       "localhost/conch/busybox:latest",
		},
	}
	svc := &Service{Image: imageOps, TemplateBootIndex: imageOps}

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
			ImageName: "boot-index",
		},
		removeErr: errors.New("remove failed"),
	}
	svc := &Service{Image: imageOps, TemplateBootIndex: imageOps}

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
	svc := &Service{Image: imageOps, TemplateBootIndex: imageOps}

	if _, err := svc.createTemplateFromSource(context.Background(), "default", "tmpl-1", TemplateCreateOptions{
		Source: "source", KernelPath: "/kernel", InitrdPath: "/initrd",
	}); err == nil {
		t.Fatal("createTemplateFromSource() error = nil, want publish failure")
	}
	if len(imageOps.removeCalls) != 0 {
		t.Fatalf("remove calls = %#v, want none before a successful publish", imageOps.removeCalls)
	}
}
