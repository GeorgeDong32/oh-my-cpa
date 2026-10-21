package sessionstats

// Shared read-only SQLite access for B-class parsers. Third-party databases
// are never written; immutable=0 keeps the owner's WAL semantics intact.

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func openReadonlySQLite(file string) (*sql.DB, error) {
	absolute, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	dsn := "file:" + url.PathEscape(absolute) + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s read-only: %w", filepath.Base(file), err)
	}
	return db, nil
}
