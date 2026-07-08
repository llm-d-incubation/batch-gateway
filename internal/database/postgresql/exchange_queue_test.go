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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/pashagolub/pgxmock/v4"

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
)

// newTestExchangeClient builds a PostgresExchangeClient backed by a pgxmock
// pool. The listener is intentionally left nil: every path exercised here is
// either non-blocking or returns items on the first drain, so it must never be
// dereferenced. The mock pool is closed automatically via t.Cleanup.
func newTestExchangeClient(t *testing.T) (*PostgresExchangeClient, pgxmock.PgxPoolIface) {
	t.Helper()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("failed to create pgxmock pool: %v", err)
	}
	t.Cleanup(mock.Close)

	client := &PostgresExchangeClient{
		pool:      mock,
		eventSubs: make(map[string]*eventSub),
		logger:    logr.Discard(),
	}

	return client, mock
}

func TestPQEnqueue(t *testing.T) {
	ctx := context.Background()

	t.Run("enqueues with TTL", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		item := &db_api.BatchJobPriority{
			ID:   "job-1",
			SLO:  time.Now().Add(time.Hour),
			Data: []byte("payload"),
			TTL:  600,
		}

		mock.ExpectExec("INSERT INTO batch_queue").
			WithArgs("job-1", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("SELECT", 1))

		if err := client.PQEnqueue(ctx, item); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("enqueues without TTL (nil expires_at)", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		item := &db_api.BatchJobPriority{
			ID:  "job-2",
			SLO: time.Now().Add(time.Hour),
		}

		mock.ExpectExec("INSERT INTO batch_queue").
			WithArgs("job-2", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("SELECT", 0))

		if err := client.PQEnqueue(ctx, item); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns error for nil item", func(t *testing.T) {
		client, _ := newTestExchangeClient(t)

		if err := client.PQEnqueue(ctx, nil); err == nil {
			t.Fatal("expected error for nil item")
		}
	})

	t.Run("returns error for invalid item (zero SLO)", func(t *testing.T) {
		client, _ := newTestExchangeClient(t)

		if err := client.PQEnqueue(ctx, &db_api.BatchJobPriority{ID: "job-3"}); err == nil {
			t.Fatal("expected error for zero SLO")
		}
	})
}

func TestPQDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("deletes matching job", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		item := &db_api.BatchJobPriority{ID: "job-1", SLO: time.Now().Add(time.Hour)}

		mock.ExpectExec("DELETE FROM batch_queue").
			WithArgs("job-1", pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("DELETE", 1))

		n, err := client.PQDelete(ctx, item)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if n != 1 {
			t.Fatalf("expected 1 deleted, got %d", n)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("no rows deleted returns 0", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		item := &db_api.BatchJobPriority{ID: "job-x", SLO: time.Now().Add(time.Hour)}

		mock.ExpectExec("DELETE FROM batch_queue").
			WithArgs("job-x", pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("DELETE", 0))

		n, err := client.PQDelete(ctx, item)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if n != 0 {
			t.Fatalf("expected 0 deleted, got %d", n)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns error for nil item", func(t *testing.T) {
		client, _ := newTestExchangeClient(t)

		if _, err := client.PQDelete(ctx, nil); err == nil {
			t.Fatal("expected error for nil item")
		}
	})
}

func TestPQDequeue(t *testing.T) {
	ctx := context.Background()

	t.Run("non-blocking drain returns items", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		slo := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		rows := pgxmock.NewRows([]string{"job_id", "slo_score", "data"}).
			AddRow("job-1", slo.UnixMicro(), []byte("payload")).
			AddRow("job-2", slo.Add(time.Second).UnixMicro(), []byte(nil))

		mock.ExpectQuery("DELETE FROM batch_queue").
			WithArgs(2).
			WillReturnRows(rows)

		items, err := client.PQDequeue(ctx, 0, 2)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("expected 2 items, got %d", len(items))
		}
		if items[0].ID != "job-1" {
			t.Errorf("expected job-1, got %s", items[0].ID)
		}
		if !items[0].SLO.Equal(slo) {
			t.Errorf("SLO round-trip mismatch: want %v, got %v", slo, items[0].SLO)
		}
		if string(items[0].Data) != "payload" {
			t.Errorf("expected data payload, got %q", items[0].Data)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("non-blocking drain with no items returns nil", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		rows := pgxmock.NewRows([]string{"job_id", "slo_score", "data"})

		mock.ExpectQuery("DELETE FROM batch_queue").
			WithArgs(5).
			WillReturnRows(rows)

		items, err := client.PQDequeue(ctx, 0, 5)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if items != nil {
			t.Fatalf("expected nil items, got %d", len(items))
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	// With a positive timeout, items available on the FIRST drain must be
	// returned immediately without ever consulting the (nil) listener.
	t.Run("blocking timeout returns first-drain items without listener", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		slo := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		rows := pgxmock.NewRows([]string{"job_id", "slo_score", "data"}).
			AddRow("job-1", slo.UnixMicro(), []byte("payload"))

		mock.ExpectQuery("DELETE FROM batch_queue").
			WithArgs(1).
			WillReturnRows(rows)

		items, err := client.PQDequeue(ctx, 5*time.Second, 1)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("expected 1 item, got %d", len(items))
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

func TestPQGetIDs(t *testing.T) {
	ctx := context.Background()

	t.Run("returns all queued IDs", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		rows := pgxmock.NewRows([]string{"job_id"}).
			AddRow("job-1").
			AddRow("job-2")

		mock.ExpectQuery("SELECT job_id FROM batch_queue").
			WillReturnRows(rows)

		ids, err := client.PQGetIDs(ctx)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(ids) != 2 || !ids["job-1"] || !ids["job-2"] {
			t.Fatalf("unexpected ids: %v", ids)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns empty map when queue empty", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		mock.ExpectQuery("SELECT job_id FROM batch_queue").
			WillReturnRows(pgxmock.NewRows([]string{"job_id"}))

		ids, err := client.PQGetIDs(ctx)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("expected empty map, got %v", ids)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}
