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

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
)

// These tests exercise the fully pgxmock-testable paths: the producer INSERT +
// pg_notify statement and the destructive per-job drain/fan-out. The NOTIFY/LISTEN
// wake path and the blocking dispatcher loop are not unit-testable with pgxmock
// (Acquire returns "not implemented"); those belong in a real-DB e2e test, matching
// the rest of this package having no real-DB unit harness.

func TestECProducerSendEvents(t *testing.T) {
	ctx := context.Background()

	t.Run("sends a single event", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		mock.ExpectExec("INSERT INTO batch_events").
			WithArgs("job-1", int(db_api.BatchEventCancel), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("SELECT", 1))

		sentIDs, err := client.ECProducerSendEvents(ctx, []db_api.BatchEvent{
			{ID: "job-1", Type: db_api.BatchEventCancel, TTL: 60},
		})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(sentIDs) != 1 || sentIDs[0] != "job-1" {
			t.Fatalf("expected sentIDs [job-1], got %v", sentIDs)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("sends multiple events in order", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		mock.ExpectExec("INSERT INTO batch_events").
			WithArgs("job-1", int(db_api.BatchEventPause), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("SELECT", 1))
		mock.ExpectExec("INSERT INTO batch_events").
			WithArgs("job-2", int(db_api.BatchEventResume), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("SELECT", 1))

		sentIDs, err := client.ECProducerSendEvents(ctx, []db_api.BatchEvent{
			{ID: "job-1", Type: db_api.BatchEventPause, TTL: 60},
			{ID: "job-2", Type: db_api.BatchEventResume, TTL: 60},
		})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(sentIDs) != 2 {
			t.Fatalf("expected 2 sentIDs, got %v", sentIDs)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns error for empty events", func(t *testing.T) {
		client, _ := newTestExchangeClient(t)

		if _, err := client.ECProducerSendEvents(ctx, nil); err == nil {
			t.Fatal("expected error for empty events")
		}
	})

	t.Run("returns error for invalid event", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		// Missing TTL fails IsValid; no Exec should be issued.
		if _, err := client.ECProducerSendEvents(ctx, []db_api.BatchEvent{
			{ID: "job-1", Type: db_api.BatchEventCancel},
		}); err == nil {
			t.Fatal("expected error for invalid event")
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns partial sentIDs on exec error", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		mock.ExpectExec("INSERT INTO batch_events").
			WithArgs("job-1", int(db_api.BatchEventCancel), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("SELECT", 1))
		mock.ExpectExec("INSERT INTO batch_events").
			WithArgs("job-2", int(db_api.BatchEventCancel), pgxmock.AnyArg()).
			WillReturnError(context.DeadlineExceeded)

		sentIDs, err := client.ECProducerSendEvents(ctx, []db_api.BatchEvent{
			{ID: "job-1", Type: db_api.BatchEventCancel, TTL: 60},
			{ID: "job-2", Type: db_api.BatchEventCancel, TTL: 60},
		})
		if err == nil {
			t.Fatal("expected error from second exec")
		}
		if len(sentIDs) != 1 || sentIDs[0] != "job-1" {
			t.Fatalf("expected partial sentIDs [job-1], got %v", sentIDs)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

func TestDeliverJobEvents(t *testing.T) {
	ctx := context.Background()

	t.Run("drains and delivers events to the subscriber in FIFO order", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		sub := &eventSub{ch: make(chan db_api.BatchEvent, eventChanBufSize)}
		client.eventSubs["job-1"] = sub

		rows := pgxmock.NewRows([]string{"event_type"}).
			AddRow(int(db_api.BatchEventPause)).
			AddRow(int(db_api.BatchEventResume))

		mock.ExpectQuery("DELETE FROM batch_events").
			WithArgs("job-1").
			WillReturnRows(rows)

		client.deliverJobEvents(ctx, "job-1")

		got := drainChan(t, sub.ch, 2)
		if got[0].Type != db_api.BatchEventPause || got[1].Type != db_api.BatchEventResume {
			t.Fatalf("expected [pause, resume], got %+v", got)
		}
		for _, ev := range got {
			if ev.ID != "job-1" {
				t.Fatalf("expected event ID job-1, got %s", ev.ID)
			}
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("no-op when the job is not subscribed", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		// No subscription registered and no query expected; must not touch the DB.
		client.deliverJobEvents(ctx, "job-unknown")

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("delivers nothing when the table has no rows", func(t *testing.T) {
		client, mock := newTestExchangeClient(t)

		sub := &eventSub{ch: make(chan db_api.BatchEvent, eventChanBufSize)}
		client.eventSubs["job-1"] = sub

		mock.ExpectQuery("DELETE FROM batch_events").
			WithArgs("job-1").
			WillReturnRows(pgxmock.NewRows([]string{"event_type"}))

		client.deliverJobEvents(ctx, "job-1")

		if len(sub.ch) != 0 {
			t.Fatalf("expected empty channel, got %d events", len(sub.ch))
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

func TestECConsumerGetChannelValidation(t *testing.T) {
	t.Run("returns error for empty ID", func(t *testing.T) {
		client, _ := newTestExchangeClient(t)

		if _, err := client.ECConsumerGetChannel(context.Background(), ""); err == nil {
			t.Fatal("expected error for empty ID")
		}
	})
}

// drainChan reads exactly n events off ch, failing if they are not immediately
// available (deliverJobEvents delivers synchronously, so no blocking is needed).
func drainChan(t *testing.T, ch chan db_api.BatchEvent, n int) []db_api.BatchEvent {
	t.Helper()
	if len(ch) != n {
		t.Fatalf("expected %d buffered events, got %d", n, len(ch))
	}
	out := make([]db_api.BatchEvent, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, <-ch)
	}
	return out
}
