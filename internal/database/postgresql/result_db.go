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

package postgresql

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/llm-d/llm-d-batch-gateway/internal/database/api"
)

//go:embed result_schema.sql
var resultSchemaSql string

// Compile-time check: resultTableDescriptor implements TableDescriptor.
var _ TableDescriptor = (*resultTableDescriptor)(nil)

// resultTableDescriptor only supplies the schema; the client issues its own
// SQL because the table is keyed by (batch_id, custom_id), not the common
// id/tenant layout pgCore builds queries for.
type resultTableDescriptor struct{}

func (resultTableDescriptor) TableName() string      { return "batch_request_results" }
func (resultTableDescriptor) Schema() string         { return resultSchemaSql }
func (resultTableDescriptor) ExtraColumns() []string { return nil }

// PostgresResultDBClient implements api.ResultDBClient using PostgreSQL.
type PostgresResultDBClient struct {
	core *pgCore
}

var _ api.ResultDBClient = (*PostgresResultDBClient)(nil)

// NewPostgresResultDBClient creates a new PostgreSQL result database client.
func NewPostgresResultDBClient(ctx context.Context, config *PostgreSQLConfig) (*PostgresResultDBClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	core, err := newPgCore(ctx, config, resultTableDescriptor{})
	if err != nil {
		return nil, err
	}

	logr.FromContextOrDiscard(ctx).Info("NewPostgresResultDBClient: client created successfully")
	return &PostgresResultDBClient{core: core}, nil
}

func (c *PostgresResultDBClient) Close() error {
	return c.core.close()
}

func (c *PostgresResultDBClient) ResultStore(ctx context.Context, row *api.ResultRow) error {
	if row == nil {
		return fmt.Errorf("row is nil")
	}
	_, err := c.core.pool.Exec(ctx,
		`INSERT INTO batch_request_results (batch_id, custom_id, is_error, line)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (batch_id, custom_id) DO NOTHING`,
		row.BatchID, row.CustomID, row.IsError, row.Line)
	if err != nil {
		return fmt.Errorf("store result row: %w", err)
	}
	return nil
}

func (c *PostgresResultDBClient) ResultGetAll(ctx context.Context, batchID string) ([]*api.ResultRow, error) {
	rows, err := c.core.pool.Query(ctx,
		`SELECT custom_id, is_error, line FROM batch_request_results WHERE batch_id = $1`, batchID)
	if err != nil {
		return nil, fmt.Errorf("get result rows: %w", err)
	}
	defer rows.Close()

	var out []*api.ResultRow
	for rows.Next() {
		r := &api.ResultRow{BatchID: batchID}
		if err := rows.Scan(&r.CustomID, &r.IsError, &r.Line); err != nil {
			return nil, fmt.Errorf("scan result row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate result rows: %w", err)
	}
	return out, nil
}

func (c *PostgresResultDBClient) ResultDelete(ctx context.Context, batchID string) error {
	_, err := c.core.pool.Exec(ctx,
		`DELETE FROM batch_request_results WHERE batch_id = $1`, batchID)
	if err != nil {
		return fmt.Errorf("delete result rows: %w", err)
	}
	return nil
}
