package db

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// migrationsFS carries the SQL into the binary, so the migrate command needs
// nothing on disk at runtime.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNoChange reports that the database was already at the requested version.
var ErrNoChange = migrate.ErrNoChange

// Migrator applies the embedded migrations.
type Migrator struct {
	migrate *migrate.Migrate
	db      *sql.DB
}

// NewMigrator connects to databaseURL and prepares the embedded migrations.
// Call Close when finished.
func NewMigrator(databaseURL string) (*Migrator, error) {
	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading embedded migrations: %w", err)
	}

	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	driver, err := pgxmigrate.WithInstance(sqlDB, &pgxmigrate.Config{})
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("preparing migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("preparing migrator: %w", err)
	}

	return &Migrator{migrate: m, db: sqlDB}, nil
}

// Close releases the migrator's database connection.
func (m *Migrator) Close() error {
	sourceErr, dbErr := m.migrate.Close()
	return errors.Join(sourceErr, dbErr, m.db.Close())
}

// Up applies every pending migration.
func (m *Migrator) Up() error {
	return m.migrate.Up()
}

// Down rolls back a single migration.
//
// golang-migrate's own Down() drops every migration, which is a surprising
// thing for `migrate down` to do, so that lives behind DownAll instead.
func (m *Migrator) Down() error {
	return m.migrate.Steps(-1)
}

// DownAll rolls back every applied migration.
func (m *Migrator) DownAll() error {
	return m.migrate.Down()
}

// Status describes what is applied and what is available.
type Status struct {
	// Version is the most recently applied migration, zero when none are.
	Version uint
	// Dirty reports that a migration failed part way and needs manual
	// attention before anything else can run.
	Dirty bool
	// Available lists every migration compiled into the binary.
	Available []string
}

// Status reports the applied version alongside the available migrations.
func (m *Migrator) Status() (Status, error) {
	available, err := availableMigrations()
	if err != nil {
		return Status{}, err
	}

	version, dirty, err := m.migrate.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return Status{Version: 0, Dirty: false, Available: available}, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("reading schema version: %w", err)
	}
	return Status{Version: version, Dirty: dirty, Available: available}, nil
}

// Force sets the recorded version without running anything, which is how a
// dirty database is recovered once the failed migration has been sorted out by
// hand.
func (m *Migrator) Force(version int) error {
	return m.migrate.Force(version)
}

func availableMigrations() ([]string, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("listing embedded migrations: %w", err)
	}

	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".up.sql") {
			names = append(names, strings.TrimSuffix(entry.Name(), ".up.sql"))
		}
	}
	sort.Strings(names)
	return names, nil
}
