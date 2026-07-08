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

	"github.com/pashagolub/pgxmock/v4"
)

func TestStatusSet(t *testing.T) {
	ctx := context.Background()

	t.Run("sets status with explicit TTL", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		data := []byte(`{"status":"in_progress"}`)

		mock.ExpectExec("INSERT INTO batch_status").
			WithArgs("job-1", data, pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))

		if err := client.StatusSet(ctx, "job-1", 3600, data); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("defaults TTL when non-positive", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		data := []byte(`{"status":"finalizing"}`)

		// expires_at is now()+statusTTLDefaultSec; matched loosely via AnyArg.
		mock.ExpectExec("INSERT INTO batch_status").
			WithArgs("job-1", data, pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))

		if err := client.StatusSet(ctx, "job-1", 0, data); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns error for empty ID", func(t *testing.T) {
		client, _ := newTestExchangeClient(t)

		if err := client.StatusSet(ctx, "", 3600, []byte("x")); err == nil {
			t.Fatal("expected error for empty ID")
		}
	})

	t.Run("returns error for empty data", func(t *testing.T) {
		client, _ := newTestExchangeClient(t)

		if err := client.StatusSet(ctx, "job-1", 3600, nil); err == nil {
			t.Fatal("expected error for empty data")
		}
	})
}

func TestStatusGet(t *testing.T) {
	ctx := context.Background()

	t.Run("returns data when present and unexpired", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		want := []byte(`{"status":"completed"}`)
		rows := pgxmock.NewRows([]string{"data"}).AddRow(want)

		mock.ExpectQuery("SELECT data FROM batch_status").
			WithArgs("job-1").
			WillReturnRows(rows)

		got, err := client.StatusGet(ctx, "job-1")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("expected %q, got %q", want, got)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns nil when missing or expired", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		rows := pgxmock.NewRows([]string{"data"})

		mock.ExpectQuery("SELECT data FROM batch_status").
			WithArgs("job-missing").
			WillReturnRows(rows)

		got, err := client.StatusGet(ctx, "job-missing")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if got != nil {
			t.Fatalf("expected nil data, got %q", got)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns error for empty ID", func(t *testing.T) {
		client, _ := newTestExchangeClient(t)

		if _, err := client.StatusGet(ctx, ""); err == nil {
			t.Fatal("expected error for empty ID")
		}
	})
}

func TestStatusDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("deletes and returns count", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		mock.ExpectExec("DELETE FROM batch_status WHERE job_id").
			WithArgs("job-1").
			WillReturnResult(pgxmock.NewResult("DELETE", 1))

		n, err := client.StatusDelete(ctx, "job-1")
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

	t.Run("returns zero when absent", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		mock.ExpectExec("DELETE FROM batch_status WHERE job_id").
			WithArgs("job-absent").
			WillReturnResult(pgxmock.NewResult("DELETE", 0))

		n, err := client.StatusDelete(ctx, "job-absent")
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

	t.Run("returns error for empty ID", func(t *testing.T) {
		client, _ := newTestExchangeClient(t)

		if _, err := client.StatusDelete(ctx, ""); err == nil {
			t.Fatal("expected error for empty ID")
		}
	})
}
