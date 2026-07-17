package template

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/openeuler/Conch/internal/daemon/state"
)

func TestStoreCreateMarkReadyAndList(t *testing.T) {
	ctx := context.Background()
	raw := newTestStateStore(t)
	store := NewStore(raw)
	store.now = func() time.Time { return time.Unix(10, 0) }

	rec, err := store.Create(ctx, CreateRequest{
		Origin:    state.TemplateOriginImage,
		Namespace: "team-a",
		ImageName: "image-ref",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !strings.HasPrefix(rec.ID, "tmpl_") {
		t.Fatalf("id = %q, want tmpl_ prefix", rec.ID)
	}
	if rec.State != state.TemplateCreating {
		t.Fatalf("state = %q", rec.State)
	}
	if mode := BootMode(rec); mode != "" {
		t.Fatalf("creating template boot mode = %q, want empty", mode)
	}

	bootIndexDigest := digest.FromString("cold boot index").String()
	if err := store.MarkReady(ctx, rec.ID, ReadyState{
		BootIndexDigest: bootIndexDigest,
		BootMode:        state.TemplateBootModeCold,
	}); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != state.TemplateReady || got.BootIndexDigest != bootIndexDigest || got.BootMode != state.TemplateBootModeCold {
		t.Fatalf("record = %#v", got)
	}
	if mode := BootMode(got); mode != state.TemplateBootModeCold {
		t.Fatalf("ready template boot mode = %q, want cold", mode)
	}

	items, err := store.List(ctx, Filter{Origin: state.TemplateOriginImage, BootMode: state.TemplateBootModeCold, Namespace: "team-a", State: state.TemplateReady})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != rec.ID {
		t.Fatalf("List() = %#v", items)
	}
}

func TestStoreMarkReadyValidatesDigestAndBootMode(t *testing.T) {
	ctx := context.Background()
	store := NewStore(newTestStateStore(t))
	rec, err := store.Create(ctx, CreateRequest{Origin: state.TemplateOriginCheckpoint})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	for _, tt := range []struct {
		name  string
		ready ReadyState
		want  string
	}{
		{
			name: "invalid digest",
			ready: ReadyState{
				BootIndexDigest: "sha256:not-a-content-digest",
				BootMode:        state.TemplateBootModeResume,
			},
			want: "invalid boot index digest",
		},
		{
			name: "invalid boot mode",
			ready: ReadyState{
				BootIndexDigest: digest.FromString("resume boot index").String(),
				BootMode:        "warm",
			},
			want: "unknown template boot mode",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := store.MarkReady(ctx, rec.ID, tt.ready)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("MarkReady() error = %v, want %q", err, tt.want)
			}
		})
	}

	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != state.TemplateCreating || got.BootIndexDigest != "" || got.BootMode != "" {
		t.Fatalf("invalid READY publication mutated record: %#v", got)
	}
}

func TestStoreReadyIdentityIsImmutable(t *testing.T) {
	ctx := context.Background()
	store := NewStore(newTestStateStore(t))
	rec, err := store.Create(ctx, CreateRequest{Origin: state.TemplateOriginImage})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	firstDigest := digest.FromString("first boot index").String()
	if err := store.MarkReady(ctx, rec.ID, ReadyState{
		BootIndexDigest: firstDigest,
		BootMode:        state.TemplateBootModeCold,
	}); err != nil {
		t.Fatalf("first MarkReady() error = %v", err)
	}
	err = store.MarkReady(ctx, rec.ID, ReadyState{
		BootIndexDigest: digest.FromString("second boot index").String(),
		BootMode:        state.TemplateBootModeResume,
	})
	if err == nil || !strings.Contains(err.Error(), "want "+state.TemplateCreating) {
		t.Fatalf("second MarkReady() error = %v, want immutable READY error", err)
	}
	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.BootIndexDigest != firstDigest || got.BootMode != state.TemplateBootModeCold || got.State != state.TemplateReady {
		t.Fatalf("READY identity changed: %#v", got)
	}
}

func TestBootModeUsesCapabilityCache(t *testing.T) {
	resume := state.TemplateRecord{
		BootMode: state.TemplateBootModeResume,
	}
	if got := BootMode(resume); got != state.TemplateBootModeResume {
		t.Fatalf("resume BootMode() = %q", got)
	}
	if got := BootMode(state.TemplateRecord{}); got != "" {
		t.Fatalf("empty BootMode() = %q", got)
	}
}

func TestStorePublishCheckpointDelegatesAtomicTransition(t *testing.T) {
	ctx := context.Background()
	raw := newTestStateStore(t)
	store := NewStore(raw)
	if err := raw.UpsertTemplate(ctx, state.TemplateRecord{
		ID:               "t1",
		Origin:           state.TemplateOriginCheckpoint,
		State:            state.TemplateCreating,
		ParentTemplateID: "t0",
	}); err != nil {
		t.Fatalf("UpsertTemplate() error = %v", err)
	}
	if err := raw.UpsertSandbox(ctx, state.SandboxRecord{
		PodSandboxID:                  "pod-1",
		SourceTemplateID:              "t0",
		SourceBootIndexDigest:         digest.FromString("source boot index").String(),
		CheckpointHeadTemplateID:      "t0",
		CheckpointHeadBootIndexDigest: digest.FromString("source boot index").String(),
	}); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	checkpointDigest := digest.FromString("checkpoint boot index").String()
	if err := store.PublishCheckpoint(ctx, state.CheckpointPublication{
		TemplateID:                  "t1",
		PodSandboxID:                "pod-1",
		BootIndexDigest:             checkpointDigest,
		BootMode:                    state.TemplateBootModeResume,
		ExpectedHeadTemplateID:      "t0",
		ExpectedHeadBootIndexDigest: digest.FromString("source boot index").String(),
	}); err != nil {
		t.Fatalf("PublishCheckpoint() error = %v", err)
	}

	templateRecord, err := raw.GetTemplate(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTemplate() error = %v", err)
	}
	sandboxRecord, err := raw.GetSandbox(ctx, "pod-1")
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if templateRecord.State != state.TemplateReady || templateRecord.BootIndexDigest != checkpointDigest || templateRecord.BootMode != state.TemplateBootModeResume {
		t.Fatalf("published template = %#v", templateRecord)
	}
	if sandboxRecord.CheckpointHeadTemplateID != "t1" || sandboxRecord.CheckpointHeadBootIndexDigest != checkpointDigest {
		t.Fatalf("published sandbox head = %#v", sandboxRecord)
	}
}

func TestStoreMarkFailed(t *testing.T) {
	ctx := context.Background()
	store := NewStore(newTestStateStore(t))
	rec, err := store.Create(ctx, CreateRequest{Origin: state.TemplateOriginCheckpoint})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.MarkFailed(ctx, rec.ID, errBoom{}); err != nil {
		t.Fatalf("MarkFailed() error = %v", err)
	}
	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != state.TemplateFailed || got.LastError != "boom" {
		t.Fatalf("record = %#v", got)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

func newTestStateStore(t *testing.T) *state.BoltStore {
	t.Helper()
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return store
}
