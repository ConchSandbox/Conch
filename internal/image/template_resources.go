package image

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
)

func RetainTemplateResources(ctx context.Context, client *containerdclient.Client, templateID, bootIndexDigest string) error {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return fmt.Errorf("%w: template id is required", ErrInvalidRequest)
	}
	retainCtx := containerdclient.NewNamespaceContext(ctx)
	desc, _, err := inspectBootIndexByDigest(retainCtx, client.ContentStore(), bootIndexDigest)
	if err != nil {
		return err
	}

	digests := make(map[string]struct{})
	handler := images.Handlers(
		images.HandlerFunc(func(_ context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
			digests[desc.Digest.String()] = struct{}{}
			return nil, nil
		}),
		images.ChildrenHandler(client.ContentStore()),
	)
	if err := images.Walk(retainCtx, handler, desc); err != nil {
		return fmt.Errorf("walk template %s content: %w", templateID, err)
	}

	manager := client.LeasesService()
	lease := leases.Lease{ID: templateLeaseID(templateID)}
	created := false
	if _, err := manager.Create(retainCtx, leases.WithID(lease.ID), leases.WithLabel("conch.io/template-id", templateID)); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return fmt.Errorf("create template lease %s: %w", lease.ID, err)
		}
	} else {
		created = true
	}
	for dgst := range digests {
		if err := manager.AddResource(retainCtx, lease, leases.Resource{Type: "content", ID: dgst}); err != nil && !errdefs.IsAlreadyExists(err) {
			if created {
				_ = manager.Delete(retainCtx, lease)
			}
			return fmt.Errorf("retain template content %s: %w", dgst, err)
		}
	}
	resources, err := manager.ListResources(retainCtx, lease)
	if err != nil {
		return fmt.Errorf("list template lease %s resources: %w", lease.ID, err)
	}
	for _, resource := range resources {
		if resource.Type != "content" {
			continue
		}
		if _, keep := digests[resource.ID]; keep {
			continue
		}
		if err := manager.DeleteResource(retainCtx, lease, resource); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("release stale template content %s: %w", resource.ID, err)
		}
	}
	return nil
}

func PlanTemplateRemoval(ctx context.Context, client *containerdclient.Client, opts TemplateRemovalOptions) (TemplateRemovalPlan, error) {
	if client == nil || client.Client == nil {
		return TemplateRemovalPlan{}, fmt.Errorf("containerd client is required")
	}
	targetDigest := strings.TrimSpace(opts.Target.BootIndexDigest)
	if targetDigest == "" {
		return TemplateRemovalPlan{}, fmt.Errorf("%w: target boot index digest is required", ErrInvalidRequest)
	}
	templateID := strings.TrimSpace(opts.Target.TemplateID)

	planCtx := containerdclient.NewNamespaceContext(ctx)
	targetInfo, err := InspectBootIndex(planCtx, client, targetDigest)
	if err != nil {
		return TemplateRemovalPlan{}, fmt.Errorf("inspect target boot index %s: %w", targetDigest, err)
	}

	retainedBuildRefs := make(map[string]struct{}, len(opts.Retained))
	retainedComponents := make(map[string]struct{})
	retainedSnapshots := make(map[string]struct{})
	seenRetainedIndexes := make(map[string]struct{})
	for _, retained := range opts.Retained {
		if buildRef := strings.TrimSpace(retained.BuildRef); buildRef != "" {
			retainedBuildRefs[buildRef] = struct{}{}
		}
		digest := strings.TrimSpace(retained.BootIndexDigest)
		if digest == "" {
			return TemplateRemovalPlan{}, fmt.Errorf("%w: retained boot index digest is required", ErrInvalidRequest)
		}
		if _, ok := seenRetainedIndexes[digest]; ok {
			continue
		}
		seenRetainedIndexes[digest] = struct{}{}
		info, inspectErr := InspectBootIndex(planCtx, client, digest)
		if inspectErr != nil {
			return TemplateRemovalPlan{}, fmt.Errorf("inspect retained boot index %s: %w", digest, inspectErr)
		}
		for _, component := range bootIndexComponents(info) {
			retainedComponents[component.desc.Digest.String()] = struct{}{}
			keys, keysErr := componentSnapshotKeys(planCtx, client.Client, component.desc)
			if keysErr != nil {
				return TemplateRemovalPlan{}, fmt.Errorf("resolve retained %s snapshot keys: %w", component.kind, keysErr)
			}
			for _, key := range keys {
				retainedSnapshots[key] = struct{}{}
			}
		}
	}

	imageNames := make(map[string]struct{})
	buildRef := strings.TrimSpace(opts.Target.BuildRef)
	if _, retained := retainedBuildRefs[buildRef]; buildRef != "" && !retained {
		item, getErr := client.ImageService().Get(planCtx, buildRef)
		switch {
		case getErr == nil && item.Target.Digest.String() == targetInfo.BootIndexDigest:
			imageNames[buildRef] = struct{}{}
		case getErr != nil && !errdefs.IsNotFound(getErr):
			return TemplateRemovalPlan{}, fmt.Errorf("lookup template image %s: %w", buildRef, getErr)
		}
	}

	snapshotDepth := make(map[string]int)
	for _, component := range bootIndexComponents(targetInfo) {
		if _, retained := retainedComponents[component.desc.Digest.String()]; retained {
			continue
		}
		name := componentImageName(component.kind, component.desc)
		item, getErr := client.ImageService().Get(planCtx, name)
		switch {
		case getErr == nil && item.Target.Digest == component.desc.Digest:
			imageNames[name] = struct{}{}
		case getErr != nil && !errdefs.IsNotFound(getErr):
			return TemplateRemovalPlan{}, fmt.Errorf("lookup component image %s: %w", name, getErr)
		}

		keys, keysErr := componentSnapshotKeys(planCtx, client.Client, component.desc)
		if keysErr != nil {
			return TemplateRemovalPlan{}, fmt.Errorf("resolve target %s snapshot keys: %w", component.kind, keysErr)
		}
		for depth, key := range keys {
			if _, retained := retainedSnapshots[key]; retained {
				continue
			}
			if snapshotDepth[key] < depth+1 {
				snapshotDepth[key] = depth + 1
			}
		}
	}

	plan := TemplateRemovalPlan{TemplateID: templateID}
	for name := range imageNames {
		plan.ImageNames = append(plan.ImageNames, name)
	}
	sort.Strings(plan.ImageNames)
	for key := range snapshotDepth {
		plan.SnapshotKeys = append(plan.SnapshotKeys, key)
	}
	sort.Slice(plan.SnapshotKeys, func(i, j int) bool {
		left, right := snapshotDepth[plan.SnapshotKeys[i]], snapshotDepth[plan.SnapshotKeys[j]]
		if left != right {
			return left > right
		}
		return plan.SnapshotKeys[i] < plan.SnapshotKeys[j]
	})
	return plan, nil
}

func ApplyTemplateRemoval(ctx context.Context, client *containerdclient.Client, plan TemplateRemovalPlan) error {
	if err := RemoveTemplateArtifacts(ctx, client, plan); err != nil {
		return err
	}
	return ReleaseTemplateResources(ctx, client, plan.TemplateID)
}

// RemoveTemplateArtifacts removes unshared snapshot references and image
// records while keeping the Template lease available for retries.
func RemoveTemplateArtifacts(ctx context.Context, client *containerdclient.Client, plan TemplateRemovalPlan) error {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	cleanupCtx := containerdclient.NewNamespaceContext(ctx)
	leaseManager := client.LeasesService()
	runtimeLease := leases.Lease{ID: containerdclient.RuntimeLeaseID()}
	resources, err := leaseManager.ListResources(cleanupCtx, runtimeLease)
	var cleanupErrors []error
	if err != nil && !errdefs.IsNotFound(err) {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("list runtime lease resources: %w", err))
	}
	leasedSnapshots := make(map[string]struct{})
	if err == nil {
		for _, resource := range resources {
			if resource.Type == "snapshots/erofs" {
				leasedSnapshots[resource.ID] = struct{}{}
			}
		}
	}
	for _, key := range plan.SnapshotKeys {
		if _, ok := leasedSnapshots[key]; !ok {
			continue
		}
		if err := leaseManager.DeleteResource(cleanupCtx, runtimeLease, leases.Resource{Type: "snapshots/erofs", ID: key}); err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			cleanupErrors = append(cleanupErrors, fmt.Errorf("release snapshot %s from runtime lease: %w", key, err))
		}
	}

	for _, name := range plan.ImageNames {
		if err := client.ImageService().Delete(cleanupCtx, name); err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove template image %s: %w", name, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

// ReleaseTemplateResources drops the Template content lease and requests GC
// after all mutable artifacts have been removed.
func ReleaseTemplateResources(ctx context.Context, client *containerdclient.Client, templateID string) error {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	cleanupCtx := containerdclient.NewNamespaceContext(ctx)
	leaseManager := client.LeasesService()
	templateID = strings.TrimSpace(templateID)
	if templateID != "" {
		err := leaseManager.Delete(cleanupCtx, leases.Lease{ID: templateLeaseID(templateID)})
		if err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("release template lease %s: %w", templateID, err)
		}
	}
	var gcLease leases.Lease
	var err error
	if templateID == "" {
		gcLease, err = leaseManager.Create(cleanupCtx, leases.WithRandomID())
	} else {
		gcLease.ID = "conch.template.gc." + templateID
		_, err = leaseManager.Create(cleanupCtx, leases.WithID(gcLease.ID))
	}
	if err != nil && !errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("start synchronous garbage collection: %w", err)
	}
	if err := leaseManager.Delete(cleanupCtx, gcLease, leases.SynchronousDelete); err != nil {
		return fmt.Errorf("finish synchronous garbage collection: %w", err)
	}
	return nil
}

func templateLeaseID(templateID string) string {
	return "conch.template." + templateID
}

type bootIndexComponent struct {
	kind string
	desc ocispec.Descriptor
}

func bootIndexComponents(info BootIndexInfo) []bootIndexComponent {
	components := []bootIndexComponent{
		{kind: KindRootfs, desc: info.RootfsDescriptor},
		{kind: KindSandbox, desc: info.SandboxDescriptor},
	}
	if info.MemDescriptor.Digest != "" {
		components = append(components, bootIndexComponent{kind: KindMemSnapshot, desc: info.MemDescriptor})
	}
	return components
}

func componentSnapshotKeys(ctx context.Context, client *containerd.Client, desc ocispec.Descriptor) ([]string, error) {
	img := containerd.NewImage(client, images.Image{
		Name:   "localhost/conch/cleanup:" + desc.Digest.Encoded(),
		Target: desc,
	})
	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(diffIDs))
	for i := range diffIDs {
		keys = append(keys, identity.ChainID(diffIDs[:i+1]).String())
	}
	return keys, nil
}
