// Package testdb gives integration tests a clean, migrated database.
//
// Tests that need one call Setup, which skips them with a clear message when
// TEST_DATABASE_URL is not set, so `go test ./...` still passes on a machine
// with no Postgres.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shanejwalsh/starhane-fm-server/db"
)

const skipMessage = "TEST_DATABASE_URL is not set, skipping integration test. " +
	"Set it to a throwaway database (see .env.example) — each run drops and recreates its schema."

var (
	setupOnce sync.Once
	setupErr  error
	schemaURL string
)

// URL returns TEST_DATABASE_URL, skipping the test when it is unset.
func URL(t *testing.T) string {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip(skipMessage)
	}
	return url
}

// Setup returns a pool connected to a freshly migrated test schema.
//
// Each test binary gets its own schema, because `go test ./...` runs packages
// in parallel and they would otherwise drop and migrate the same tables
// underneath one another. The schema is rebuilt once per binary; each caller
// then gets empty tables, which gives the same isolation as rebuilding per test
// without the cost.
func Setup(t *testing.T) *pgxpool.Pool {
	t.Helper()

	base := URL(t)

	setupOnce.Do(func() { schemaURL, setupErr = resetSchema(base, schemaName()) })
	if setupErr != nil {
		t.Fatalf("preparing test database: %v", setupErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, schemaURL)
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	Truncate(t, pool)
	return pool
}

// Truncate empties every table, leaving the schema in place.
func Truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// episodes and podcasts reference feeds, so one statement with CASCADE.
	if _, err := pool.Exec(ctx, `TRUNCATE episodes, podcasts, feeds RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncating test database: %v", err)
	}
}

// schemaName derives a Postgres schema name from the test binary, so each
// package gets its own and parallel packages cannot collide.
func schemaName() string {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".test")

	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, name)

	if cleaned == "" {
		cleaned = "anon"
	}
	return "test_" + cleaned
}

// resetSchema drops and recreates the binary's schema, then migrates into it,
// so a run can never be affected by a half-migrated or hand-edited schema. It
// returns the connection URL scoped to that schema.
func resetSchema(baseURL, schema string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		return "", err
	}
	// The schema name is derived from the binary name and contains only
	// [a-z0-9_], but quote it anyway rather than interpolating bare.
	_, err = pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE; CREATE SCHEMA %q`, schema, schema))
	pool.Close()
	if err != nil {
		return "", err
	}

	scoped, err := withSearchPath(baseURL, schema)
	if err != nil {
		return "", err
	}

	migrator, err := db.NewMigrator(scoped)
	if err != nil {
		return "", err
	}
	defer migrator.Close()

	if err := migrator.Up(); err != nil {
		return "", err
	}
	return scoped, nil
}

// withSearchPath scopes a connection URL to a schema, so migrations and queries
// both land there rather than in public.
func withSearchPath(rawURL, schema string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
