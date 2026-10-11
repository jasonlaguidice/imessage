package connector

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/util/dbutil"
)

// testPGURIEnv names the environment variable that enables the PostgreSQL
// half of the store SQL tests. It must point at a disposable server, e.g.
//
//	docker run -d --rm --name imsync-pgtest -e POSTGRES_PASSWORD=test \
//	  -p 127.0.0.1:55432:5432 postgres:16-alpine
//	IMESSAGE_TEST_PG_URI='postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable' \
//	  go test ./pkg/connector/ -run 'Store' -v
//
// Each test gets its own freshly created schema, dropped on cleanup, so the
// tests never see each other's rows or leave anything behind.
const testPGURIEnv = "IMESSAGE_TEST_PG_URI"

// forEachTestDB runs fn once against a fresh SQLite database and, when
// IMESSAGE_TEST_PG_URI is set, once more against a fresh PostgreSQL schema.
// Both are wrapped in dbutil exactly as the bridge does, so placeholder
// rewriting and dialect detection behave as in production.
func forEachTestDB(t *testing.T, fn func(t *testing.T, db *dbutil.Database)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		fn(t, newTestSQLiteDB(t))
	})
	t.Run("postgres", func(t *testing.T) {
		uri := os.Getenv(testPGURIEnv)
		if uri == "" {
			t.Skipf("%s not set", testPGURIEnv)
		}
		fn(t, newTestPostgresDB(t, uri))
	})
}

func newTestSQLiteDB(t *testing.T) *dbutil.Database {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	raw, err := sql.Open("sqlite3", "file:"+path+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// One connection keeps the raw *sql.Tx batch paths and dbutil's own
	// queries from contending for the write lock inside a single test.
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = raw.Close() })
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	if err != nil {
		t.Fatalf("wrap sqlite in dbutil: %v", err)
	}
	if db.Dialect != dbutil.SQLite {
		t.Fatalf("expected SQLite dialect, got %v", db.Dialect)
	}
	return db
}

func newTestPostgresDB(t *testing.T, baseURI string) *dbutil.Database {
	t.Helper()
	admin, err := sql.Open("postgres", baseURI)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	schema := fmt.Sprintf("imtest_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })

	u, err := url.Parse(baseURI)
	if err != nil {
		t.Fatalf("parse %s: %v", testPGURIEnv, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	raw, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatalf("open postgres schema: %v", err)
	}
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = raw.Close() })
	db, err := dbutil.NewWithDB(raw, "postgres")
	if err != nil {
		t.Fatalf("wrap postgres in dbutil: %v", err)
	}
	if db.Dialect != dbutil.Postgres {
		t.Fatalf("expected Postgres dialect, got %v", db.Dialect)
	}
	return db
}
