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
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestInFlightSet(t *testing.T) {
	ctx := context.Background()

	t.Run("upserts entry successfully", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		mock.ExpectExec("INSERT INTO batch_inflight").
			WithArgs("job-1", "proc-1", pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))

		if err := client.InFlightSet(ctx, "job-1", "proc-1"); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns error for empty jobID", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		if err := client.InFlightSet(ctx, "", "proc-1"); err == nil {
			t.Fatal("expected error for empty jobID, got nil")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns error for empty processorID", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		if err := client.InFlightSet(ctx, "job-1", ""); err == nil {
			t.Fatal("expected error for empty processorID, got nil")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("propagates exec error", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		wantErr := errors.New("boom")
		mock.ExpectExec("INSERT INTO batch_inflight").
			WithArgs("job-1", "proc-1", pgxmock.AnyArg()).
			WillReturnError(wantErr)

		err := client.InFlightSet(ctx, "job-1", "proc-1")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected wrapped %v, got %v", wantErr, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

func TestInFlightDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("deletes entry successfully", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		mock.ExpectExec("DELETE FROM batch_inflight").
			WithArgs("job-1").
			WillReturnResult(pgxmock.NewResult("DELETE", 1))

		if err := client.InFlightDelete(ctx, "job-1"); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns error for empty jobID", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		if err := client.InFlightDelete(ctx, ""); err == nil {
			t.Fatal("expected error for empty jobID, got nil")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("propagates exec error", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		wantErr := errors.New("boom")
		mock.ExpectExec("DELETE FROM batch_inflight").
			WithArgs("job-1").
			WillReturnError(wantErr)

		err := client.InFlightDelete(ctx, "job-1")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected wrapped %v, got %v", wantErr, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

func TestInFlightGetAll(t *testing.T) {
	ctx := context.Background()

	t.Run("returns all entries", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		rows := pgxmock.NewRows([]string{"job_id", "processor_id", "last_seen"}).
			AddRow("job-1", "proc-1", int64(100)).
			AddRow("job-2", "proc-2", int64(200))

		mock.ExpectQuery("SELECT job_id, processor_id, last_seen FROM batch_inflight").
			WillReturnRows(rows)

		got, err := client.InFlightGetAll(ctx)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(got))
		}
		e1, ok := got["job-1"]
		if !ok {
			t.Fatal("missing job-1")
		}
		if e1.ProcessorID != "proc-1" || e1.LastSeen != 100 {
			t.Fatalf("unexpected entry for job-1: %+v", e1)
		}
		e2, ok := got["job-2"]
		if !ok {
			t.Fatal("missing job-2")
		}
		if e2.ProcessorID != "proc-2" || e2.LastSeen != 200 {
			t.Fatalf("unexpected entry for job-2: %+v", e2)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns empty non-nil map when no rows", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		rows := pgxmock.NewRows([]string{"job_id", "processor_id", "last_seen"})

		mock.ExpectQuery("SELECT job_id, processor_id, last_seen FROM batch_inflight").
			WillReturnRows(rows)

		got, err := client.InFlightGetAll(ctx)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if got == nil {
			t.Fatal("expected non-nil map, got nil")
		}
		if len(got) != 0 {
			t.Fatalf("expected empty map, got %d entries", len(got))
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("propagates query error", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		wantErr := errors.New("boom")
		mock.ExpectQuery("SELECT job_id, processor_id, last_seen FROM batch_inflight").
			WillReturnError(wantErr)

		_, err := client.InFlightGetAll(ctx)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected wrapped %v, got %v", wantErr, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}
