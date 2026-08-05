# Template Module Interface

Status: Draft

## Purpose

The Template module is a metadata catalog for sandbox templates. It provides
`Create`, `Get`, `List`, and `Delete` operations.

## Domain Model

### Entry

`Entry` describes a Template through its identity, boot configuration, lineage,
provenance, and creation metadata.

| Field | Type | Description | Constraints |
| --- | --- | --- | --- |
| `ID` | `string` | Uniquely identifies the Template record. | Required. |
| `Origin` | `Origin` | Identifies the process that produced the Template. | Required. |
| `BootMode` | `BootMode` | Identifies how the Template starts a Sandbox. | Required. |
| `BootIndexDigest` | `string` | References the OCI Boot Index associated with the Template. | Required; must be a valid OCI digest. |
| `ParentTemplateID` | `string` | Identifies the parent Template when this record derives from another Template. | Optional. |
| `SourceSandboxID` | `string` | Identifies the Sandbox used to produce the Template. | Optional. |
| `ImageName` | `string` | Records the source image name associated with the Template. | Optional. |
| `BuildRef` | `string` | Records the external build reference associated with the Template. | Optional. |
| `Labels` | `map[string]string` | Stores caller-defined metadata as key-value pairs. | Optional. |
| `CreatedAt` | `int64` | Records when the Template record was created. | Unix nanoseconds; assigned by `Create` when zero. |

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
| [`Get`](#get) | Retrieves a Template record by ID. |
| [`List`](#list) | Lists Template records using optional filters. |
| [`Delete`](#delete) | Deletes a Template record by ID. |

### Create

```go
Create(ctx context.Context, entry Entry) (Entry, error)
```

Creates a new Template record after validating the `Entry` constraints above.
A zero `CreatedAt` is set to the current Unix time in nanoseconds.

The record is inserted atomically. An existing record is never overwritten; a
duplicate `ID` returns `ErrAlreadyExists`. On success, `Create` returns the
normalized record that was stored.

### Get

```go
Get(ctx context.Context, id string) (Entry, error)
```

Returns the Template record identified by `id`. The returned `Entry` is
normalized using the same field rules as `Create`.

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
Delete(ctx context.Context, id string) error
```

Deletes the Template record identified by `id`. The operation is idempotent:
deleting an unknown `id` succeeds without changing stored data.

`Delete` returns an error when the storage operation cannot be completed.
