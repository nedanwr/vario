// Package sources defines the contract between source adapters and the collector.
package sources

import (
	"context"

	"github.com/nedanwr/vario/internal/model"
)

// Source is a local AI application whose data Vario collects.
//
// The collector persists the cursor returned by Snapshot only after every
// emitted batch has been committed, and persists a ChangeSet's cursor in the
// same transaction as its records.
type Source interface {
	Kind() string
	// ID is stable for a given installation and scopes every record identifier.
	ID() string
	Location() string
	// Snapshot emits the complete current state. prevCursor is the last
	// persisted cursor (empty on first run); includeHistory additionally emits
	// immutable historical events up to the snapshot boundary.
	Snapshot(ctx context.Context, prevCursor string, includeHistory bool, emit func([]model.Record) error) (cursor string, err error)
	// Changes returns records changed after cursor.
	Changes(ctx context.Context, cursor string, limit int) (ChangeSet, error)
	Close() error
}

type ChangeSet struct {
	Records []model.Record
	Cursor  string
	// More is set when further changes are immediately available.
	More bool
	// Reconcile is set when the cursor can no longer be trusted (for example the
	// source database was replaced or events after the cursor were compacted).
	Reconcile string
}

// Detection describes a discovered source installation. Detection never modifies the source.
type Detection struct {
	Kind       string
	Path       string
	Exists     bool
	Compatible bool
	Detail     string
	Facts      []Fact
}

type Fact struct{ Name, Value string }
