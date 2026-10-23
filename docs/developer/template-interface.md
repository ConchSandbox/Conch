# Template Module Interface

Status: Implemented

## Purpose

The Template module manages Conch Boot Indexes as containerd image records.
User-visible names are placed in a reserved record keyspace:

```text
logical:  registry.example:5000/team/busybox:latest
platform: linux/arm64
internal: io.conch.template/registry.example:5000/team/busybox:latest#linux/arm64
```

The record target digest is exposed as the Template ID. Record updates and
content garbage collection follow normal containerd semantics.

## Domain Model

### Entry

| Field | Type | Description | Constraints |
| --- | --- | --- | --- |
| `Name` | `string` | User-visible mutable logical Template Name. | Required. |
| `Platform` | `string` | Platform component of the local identity. | Canonical `linux/amd64` or `linux/arm64`; inferred from content by `Put` if omitted. |
| `BootIndexDigest` | `string` | Current immutable Template ID and image-record target digest. | Required; valid OCI digest. |
| `Origin` | `Origin` | Process that produced the current target. | Required. |
| `BootMode` | `BootMode` | How the current target starts a Sandbox. | Required. |
| `ParentBootIndexDigest` | `string` | Parent ID for a checkpoint target. | Optional. |
| `SourceSandboxID` | `string` | Sandbox that produced a checkpoint target. | Optional. |
| `SourceRef` | `string` | Registry or rootfs source associated with the current target. | Optional. |
| `Labels` | `map[string]string` | Caller-defined, Name-scoped metadata. | Optional. |
| `CreatedAt` | `int64` | Time at which the Name record was first created. | Unix nanoseconds. |

`Origin`, lineage, provenance, and user labels describe the Name's current
target. They are replaced when the Name moves and are not retained as history.

#### Origin

| Value | Meaning |
| --- | --- |
| `image` | Built from an OCI rootfs image or pulled as a cold Boot Index. |
| `checkpoint` | Produced by checkpoint, or pulled as a resume Boot Index. |

#### BootMode

| Value | Meaning |
| --- | --- |
| `cold` | Starts without saved memory state. |
| `resume` | Restores saved memory state. |

## Store Interface

```go
type Store interface {
    Put(context.Context, Entry, ocispec.Descriptor) (Entry, error)
    Get(context.Context, string, ...string) (Entry, error)
    List(context.Context, Filter) ([]Entry, error)
    Delete(context.Context, string, ...string) error
}
```

### Put

`Put` validates the Entry and Boot Index, then creates or updates the named
Template record. Updating one `(Name, Platform)` does not affect other names or
platforms. The platform is checked against all Boot Index component configs;
descriptor platform metadata must agree with the config. Mixed platforms fail
validation. New components carry platform metadata on their descriptors too.

Legacy records without the `#platform` suffix remain readable. Their platform
comes from `io.conch.template.platform` when available, otherwise from their
Boot Index content. Updating a legacy record on its existing platform reuses
that record; a different platform gets its own qualified record.

### Get

`Get` resolves a Template Name, validates the image record schema, target digest
and metadata labels, and returns the current Entry. The optional platform
selector defaults to the daemon's platform. A missing platform variant returns
`ErrNotFound` even if another platform exists under the same name.

### List

`List` enumerates image records carrying the Template schema label. Optional
`Origin`, `BootMode` and `Platform` filters apply to the current target metadata.
Omitting the platform filter lists all platforms.

### Delete

`Delete` removes only the selected `(Name, Platform)` image record. Its optional
platform selector follows `Get`. It returns `ErrNotFound` when the selected variant
does not exist or when the record's target changes concurrently. Content
reclamation is asynchronous and follows normal containerd GC rules.

## API and CLI platform selection

The create multipart metadata and pull/push/unpack/list/inspect/remove JSON
requests accept optional `platform`. Template records expose canonical
`platform`, and `template ls` / `inspect` display it. CLI commands accept
`--platform linux/amd64` or `--platform linux/arm64` before positional arguments.

Pull accepts a Boot Index or an outer OCI image index whose platform descriptors
point to Boot Indexes. The outer index must contain exactly one matching child.
The selected Boot Index's complete content closure is fetched under a lease;
unselected platform closures are skipped. The immutable Template ID remains the
selected Boot Index digest, rather than the outer index digest.

Create selects the source rootfs platform during fetch and EROFS conversion,
and uses the requested platform for kernel/initrd component configs. Foreign
platform conversion skips snapshot unpacking. Kernel/initrd inputs must already
target the requested platform. Push publishes a single selected Boot Index and
does not merge registry platform indexes. Unpack and runtime boot reject foreign
platforms before creating snapshots.
