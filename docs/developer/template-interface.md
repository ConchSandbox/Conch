# Template Module Interface

Status: Implemented

## Purpose

The Template module is a metadata catalog for sandbox templates. It provides
`Create`, `Get`, `List`, and `Delete` operations.

## Domain Model

### Entry

`Entry` describes a Template through its boot configuration, lineage,
provenance, and creation metadata. A Template has no independent identifier:
its identity and storage key are its immutable Boot Index digest.

| Field | Type | Description | Constraints |
| --- | --- | --- | --- |
| `Origin` | `Origin` | Identifies the process that produced the Template. | Required. |
| `BootMode` | `BootMode` | Identifies how the Template starts a Sandbox. | Required. |
| `BootIndexDigest` | `string` | Identifies the Template and references its OCI Boot Index. | Required; must be a valid OCI digest. |
| `ParentBootIndexDigest` | `string` | Identifies the parent Template when this record derives from another Template. | Optional. |
| `SourceSandboxID` | `string` | Identifies the Sandbox used to produce the Template. | Optional. |
| `SourceRef` | `string` | Records the registry reference supplied to `pull`. | Optional. |
| `Labels` | `map[string]string` | Stores caller-defined metadata as key-value pairs. | Optional. |
| `CreatedAt` | `int64` | Records when the Template record was created. | Unix nanoseconds; derived from the canonical containerd image record. |

Template metadata is stored as labels on a canonical containerd image record
whose name is derived from the digest:
`localhost/conch/template:<algorithm>-<encoded-digest>`.

#### Origin

| Value | Description |
| --- | --- |
| `image` | The Template was built from an Image. |
| `checkpoint` | The Template was captured from a Sandbox. |

#### BootMode

| Value | Description |
| --- | --- |
| `cold` | Starts a Sandbox without saved memory state. |
| `resume` | Starts a Sandbox by restoring saved memory state. |

### Filter

| Field | When empty |
| --- | --- |
| `Origin` | Matches all origins. |
| `BootMode` | Matches all boot modes. |

## Store Interface

`Store` provides the Template operations described below.

| Operation | Summary |
| --- | --- |
| [`Create`](#create) | Creates a Template record. |
| [`Get`](#get) | Retrieves a Template record by Boot Index digest. |
| [`List`](#list) | Lists Template records using optional filters. |
| [`Delete`](#delete) | Deletes a Template record by Boot Index digest. |

### Create

```go
Create(ctx context.Context, entry Entry, target ocispec.Descriptor) (Entry, error)
```

Creates a new Template record after validating the `Entry` constraints above
and confirming that `target` is a valid Boot Index whose digest and boot mode
match the `Entry`.

The store records the Boot Index child references for containerd GC and inserts
the digest-derived canonical image record atomically. An existing record is
never overwritten; a duplicate digest returns `ErrAlreadyExists`. On success,
`Create` returns the normalized record with `CreatedAt` derived from the image
record.

### Get

```go
Get(ctx context.Context, bootIndexDigest string) (Entry, error)
```

Returns the Template record identified by `bootIndexDigest`. The returned
`Entry` is normalized using the same field rules as `Create`.

`Get` returns an error when the record does not exist, cannot be read, or
contains invalid data.

### List

```go
List(ctx context.Context, filter Filter) ([]Entry, error)
```

Returns Template records that match every non-empty field in `filter`. An empty
`Filter` returns all records. Invalid non-empty `Origin` or `BootMode` values
return an error.

Every returned `Entry` is normalized using the same field rules as `Create`.

### Delete

```go
Delete(ctx context.Context, bootIndexDigest string) error
```

Deletes the digest-derived canonical image record identified by
`bootIndexDigest`. The operation is idempotent: deleting an unknown digest
succeeds without changing stored data. The delete is conditional on the record
still targeting the requested digest.

`Delete` returns an error when the storage operation cannot be completed.
Containerd GC decides when content that is no longer referenced by an image
record or another GC root is reclaimed.
