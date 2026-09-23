// Package testdb gives integration tests a clean, migrated database.
//
// Tests that need one call Setup, which skips them with a clear message when
// TEST_DATABASE_URL is not set, so `go test ./...` still passes on a machine
// with no Postgres.
package testdb

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shanejwalsh/starhane-fm-server/db"
)

const skipMessage = "TEST_DATABASE_URL is not set, skipping integration test. " +
	"Set it to a throwaway database (see .env.example) — each run drops and recreates its schema."

var (
	migrateOnce sync.Once
	migrateErr  error
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

// Setup returns a pool connected to a freshly migrated test database.
//
// The schema is dropped and rebuilt once per test binary; each caller then gets
// empty tables. Rebuilding per test would be needlessly slow, and truncating
// gives the same isolation.
func Setup(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := URL(t)

	migrateOnce.Do(func() { migrateErr = resetSchema(url) })
	if migrateErr != nil {
		t.Fatalf("preparing test database: %v", migrateErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
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
	_, err := pool.Exec(ctx, `TRUNCATE episodes, podcasts, feeds RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncating test database: %v", err)
	}
}

// resetSchema drops everything and reapplies the migrations, so a test run can
// never be affected by a half-migrated or hand-edited schema.
func resetSchema(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		pool.Close()
		return err
	}
	pool.Close()

	migrator, err := db.NewMigrator(url)
	if err != nil {
		return err
	}
	defer migrator.Close()

	return migrator.Up()
}
