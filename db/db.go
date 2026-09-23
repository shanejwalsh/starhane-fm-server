// Package db builds the Postgres connection pool and applies the embedded
// migrations.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shanejwalsh/starhane-fm-server/config"
)

// NewPool connects to Postgres and verifies the connection.
//
// Everything about how to connect — host, credentials, sslmode — comes from the
// connection string, so the same code works against a local socket and a
// managed database without special cases.
func NewPool(ctx context.Context, cfg config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing DATABASE_URL: %w", err)
	}

	poolCfg.MaxConns = cfg.DB.MaxConns
	poolCfg.MinConns = cfg.DB.MinConns
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 30 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	logger.InfoContext(ctx, "database pool ready", poolAttrs(pingCtx, pool, poolCfg)...)
	return pool, nil
}

// poolAttrs reports how the pool is sized against what the server allows. The
// API and crawler share one database's connection limit, so the server's
// max_connections is worth having in the logs of both.
func poolAttrs(ctx context.Context, pool *pgxpool.Pool, cfg *pgxpool.Config) []any {
	attrs := []any{
		slog.Int("pool_max_conns", int(cfg.MaxConns)),
		slog.Int("pool_min_conns", int(cfg.MinConns)),
		slog.String("database", cfg.ConnConfig.Database),
	}

	var serverMaxConns, serverVersion string
	if err := pool.QueryRow(ctx, "SHOW max_connections").Scan(&serverMaxConns); err == nil {
		attrs = append(attrs, slog.String("server_max_connections", serverMaxConns))
	}
	if err := pool.QueryRow(ctx, "SHOW server_version").Scan(&serverVersion); err == nil {
		attrs = append(attrs, slog.String("server_version", serverVersion))
	}
	return attrs
}
