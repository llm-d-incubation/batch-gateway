/*
Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This file provides the PostgreSQL connection configuration and pool management.

package postgresql

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgxPool is the subset of pgxpool.Pool used by the database clients.
// It is satisfied by *pgxpool.Pool and by pgxmock for testing.
type pgxPool interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Ping(ctx context.Context) error
	Close()
}

// PostgreSQLConfig holds the configuration for a PostgreSQL connection.
type PostgreSQLConfig struct {
	// Url is a PostgreSQL connection string (e.g., "postgres://user:pass@host:5432/dbname?sslmode=disable").
	Url string
}

// Validate checks that the required configuration fields are set.
func (c *PostgreSQLConfig) Validate() error {
	if c.Url == "" {
		return fmt.Errorf("url is required")
	}
	return nil
}

// newPool creates a new pgxpool.Pool from a PostgreSQLConfig.
func newPool(ctx context.Context, config *PostgreSQLConfig) (pgxPool, error) {
	if config == nil {
		return nil, fmt.Errorf("config is nil")
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	pool, err := pgxpool.New(ctx, config.Url)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return pool, nil
}
