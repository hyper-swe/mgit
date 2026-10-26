package index

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/hyper-swe/mgit/internal/model"
)

// ReadSandboxRegistrations inspects an existing daemon-owned registry without
// opening a writer, running migrations, or changing its journal mode. Callers
// must report an unreadable/unsupported registry as unknown, never as unused.
// Refs: MGIT-286
func ReadSandboxRegistrations(ctx context.Context, dbPath string) ([]model.SandboxRegistration, error) {
	absolute, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("registry path: %w", err)
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	query := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)", "foreign_keys(1)", "busy_timeout(5000)"}}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, fmt.Errorf("open read-only sandbox registry: %w", err)
	}
	defer db.Close() //nolint:errcheck // read-only inspection owns no pending writes
	var version int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version); err != nil {
		return nil, fmt.Errorf("read sandbox registry schema: %w", err)
	}
	if version != schemaVersion {
		return nil, fmt.Errorf("unsupported sandbox registry schema %d (expected %d)", version, schemaVersion)
	}
	// Reuse the owning store's decoder; no duplicate schema or state vocabulary.
	reader := Store{readDB: db}
	return reader.ListSandboxes(ctx)
}
