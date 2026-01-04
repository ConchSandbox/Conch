package snapshot

// SnapshotLocator represents a locator for identifying a snapshot with its namespace, key, and optional parent
type SnapshotLocator struct {
	Namespace string
	Key       string
	Parent    string
}

// NewSnapshotLocator creates a new snapshot locator
func NewSnapshotLocator(namespace, key, parent string) SnapshotLocator {
	return SnapshotLocator{
		Namespace: namespace,
		Key:       key,
		Parent:    parent,
	}
}
