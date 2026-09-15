# Template Module Interface

Status: Implemented

## Purpose

The Template module manages Conch Boot Indexes as containerd image records.
User-visible names are placed in a reserved record keyspace:

```text
logical:  registry.example:5000/team/busybox:latest
internal: io.conch.template/registry.example:5000/team/busybox:latest
```

The record target digest is exposed as the Template ID. Record updates and
content garbage collection follow normal containerd semantics.

## Domain Model

### Entry

| Field | Type | Description | Constraints |
| --- | --- | --- | --- |
| `Name` | `string` | User-visible mutable logical Template Name. | Required. |
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

Resume Boot Indexes published by checkpoint record `io.conch.cpu-count` and
`io.conch.memory-size-mb` on both the index and its `mem-snapshot` descriptor.
Each value is a positive integer and the two copies must agree. The CPU count
describes the captured runtime; it does not resize a restored VM. Admission
uses these values before reserving capacity, and API responses and Scheduler
reports describe the same allocation. Cold templates use the create request's
resources and do not carry these captured-resource annotations.

`BootIndexInfo.CPUCount == 0` means an older resume artifact has no CPU
metadata. Inspection can still read that artifact, but E2B or capacity-managed
creation must reject it instead of substituting the Node's default CPU count.
Rebuilding the checkpoint produces the required metadata; no migration is
performed.

## Store Interface

```go
type Store interface {
    Put(context.Context, Entry, ocispec.Descriptor) (Entry, error)
    Get(context.Context, string) (Entry, error)
    List(context.Context, Filter) ([]Entry, error)
    Delete(context.Context, string) error
}
```

### Put

`Put` validates the Entry and Boot Index, then creates or updates the named
Template record. Updating one Name does not affect other Names.

### Get

`Get` resolves a Template Name, validates the image record schema, target digest
and metadata labels, and returns the current Entry.

### List

`List` enumerates image records carrying the Template schema label. Optional
`Origin` and `BootMode` filters apply to the current target metadata.

### Delete

`Delete` removes the named image record. It returns `ErrNotFound` when the Name
does not exist or when the record's target changes concurrently. Content
reclamation is asynchronous and follows normal containerd GC rules.
