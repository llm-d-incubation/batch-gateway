//go:build integration

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

package integration

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5"

	dbapi "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	"github.com/llm-d/llm-d-batch-gateway/internal/database/postgresql"
)

// These tests exercise the Postgres exchange backend against a REAL database —
// the paths that are deliberately not unit-testable under pgxmock:
// LISTEN/NOTIFY blocking-dequeue wakeup, SKIP LOCKED exclusivity under
// contention, durable late-attach event delivery, and TTL expiry/sweep.
//
// Run locally with: make test-integration-postgres
// Or point POSTGRES_TEST_URL at any Postgres whose user can CREATE DATABASE.
//
// Timing note: the exchange client's poll fallback fires every ~1s
// (pollIntervalDefault). The NOTIFY wakeup test below is calibrated against
// that interval to prove wakeups arrive via NOTIFY, not the fallback tick.
const pollFallbackInterval = 1 * time.Second

// newExchangeTestClient creates a throwaway database on the server named by
// POSTGRES_TEST_URL, opens a PostgresExchangeClient against it (which applies
// exchange_schema.sql), and tears both down on test cleanup. Each test gets a
// fresh database so the shared batch_queue/batch_events/batch_status tables
// can never leak state across tests.
func newExchangeTestClient(t testing.TB) *postgresql.PostgresExchangeClient {
	t.Helper()

	baseURL := os.Getenv("POSTGRES_TEST_URL")
	if baseURL == "" {
		t.Skip("POSTGRES_TEST_URL not set, skipping Postgres exchange integration test")
	}

	ctx := context.Background()

	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect to POSTGRES_TEST_URL: %v", err)
	}

	dbName := fmt.Sprintf("batchgw_it_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close(ctx)
		t.Fatalf("create test database (user needs CREATEDB): %v", err)
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse POSTGRES_TEST_URL: %v", err)
	}
	u.Path = "/" + dbName

	client, err := postgresql.NewPostgresExchangeClient(ctx,
		&postgresql.PostgreSQLConfig{Url: u.String()}, logr.Discard())
	if err != nil {
		admin.Close(ctx)
		t.Fatalf("NewPostgresExchangeClient: %v", err)
	}

	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("client.Close: %v", err)
		}
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cctx, "DROP DATABASE "+dbName+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", dbName, err)
		}
		admin.Close(cctx)
	})

	return client
}

func testJob(id string, slo time.Time) *dbapi.BatchJobPriority {
	return &dbapi.BatchJobPriority{ID: id, SLO: slo}
}

func TestPostgresExchangeQueue(t *testing.T) {
	t.Run("dequeue returns items in SLO priority order", func(t *testing.T) {
		client := newExchangeTestClient(t)
		ctx := context.Background()

		base := time.Now().UTC().Truncate(time.Microsecond)
		// Enqueue deliberately out of priority order.
		for _, off := range []time.Duration{3 * time.Hour, 1 * time.Hour, 2 * time.Hour} {
			if err := client.PQEnqueue(ctx, testJob(fmt.Sprintf("job-%s", off), base.Add(off))); err != nil {
				t.Fatalf("PQEnqueue: %v", err)
			}
		}

		got, err := client.PQDequeue(ctx, 0, 3)
		if err != nil {
			t.Fatalf("PQDequeue: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("expected 3 items, got %d", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i].SLO.Before(got[i-1].SLO) {
				t.Errorf("priority order violated: item %d SLO %v before item %d SLO %v",
					i, got[i].SLO, i-1, got[i-1].SLO)
			}
		}
	})

	t.Run("blocking dequeue is woken by NOTIFY not the poll fallback", func(t *testing.T) {
		client := newExchangeTestClient(t)
		ctx := context.Background()

		type result struct {
			items []*dbapi.BatchJobPriority
			err   error
		}
		done := make(chan result, 1)
		go func() {
			items, err := client.PQDequeue(ctx, 10*time.Second, 1)
			done <- result{items: items, err: err}
		}()

		// Wait past the first poll-fallback tick so the consumer is provably
		// parked, then enqueue mid-interval: the next fallback tick is ~700ms
		// away, so receipt within 500ms can only be a NOTIFY wakeup.
		time.Sleep(pollFallbackInterval + 300*time.Millisecond)
		select {
		case r := <-done:
			t.Fatalf("dequeue returned before enqueue: items=%v err=%v", r.items, r.err)
		default:
		}

		enqueuedAt := time.Now()
		if err := client.PQEnqueue(ctx, testJob("wake-job", time.Now().UTC().Add(time.Hour))); err != nil {
			t.Fatalf("PQEnqueue: %v", err)
		}

		select {
		case r := <-done:
			if r.err != nil {
				t.Fatalf("PQDequeue: %v", r.err)
			}
			if len(r.items) != 1 || r.items[0].ID != "wake-job" {
				t.Fatalf("expected [wake-job], got %v", r.items)
			}
			if wake := time.Since(enqueuedAt); wake > 500*time.Millisecond {
				t.Errorf("wakeup took %v — likely the poll fallback fired, not NOTIFY", wake)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("blocking dequeue never woke up after enqueue")
		}
	})

	t.Run("concurrent consumers dequeue every job exactly once", func(t *testing.T) {
		client := newExchangeTestClient(t)
		ctx := context.Background()

		const nJobs = 200
		const nConsumers = 8

		base := time.Now().UTC()
		for i := 0; i < nJobs; i++ {
			if err := client.PQEnqueue(ctx, testJob(fmt.Sprintf("job-%03d", i), base.Add(time.Duration(i)*time.Second))); err != nil {
				t.Fatalf("PQEnqueue job %d: %v", i, err)
			}
		}

		var mu sync.Mutex
		seen := make(map[string]int, nJobs)
		var wg sync.WaitGroup
		for c := 0; c < nConsumers; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				misses := 0
				for misses < 3 {
					items, err := client.PQDequeue(ctx, 0, 7)
					if err != nil {
						t.Errorf("PQDequeue: %v", err)
						return
					}
					if len(items) == 0 {
						misses++
						continue
					}
					misses = 0
					mu.Lock()
					for _, it := range items {
						seen[it.ID]++
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		if len(seen) != nJobs {
			t.Errorf("expected %d distinct jobs dequeued, got %d", nJobs, len(seen))
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("job %s dequeued %d times — destructive-dequeue contract violated", id, n)
			}
		}

		ids, err := client.PQGetIDs(ctx)
		if err != nil {
			t.Fatalf("PQGetIDs: %v", err)
		}
		if len(ids) != 0 {
			t.Errorf("queue should be empty after drain, %d ids remain", len(ids))
		}
	})

	t.Run("delete removes a queued job", func(t *testing.T) {
		client := newExchangeTestClient(t)
		ctx := context.Background()

		job := testJob("doomed", time.Now().UTC().Add(time.Hour))
		if err := client.PQEnqueue(ctx, job); err != nil {
			t.Fatalf("PQEnqueue: %v", err)
		}
		n, err := client.PQDelete(ctx, job)
		if err != nil {
			t.Fatalf("PQDelete: %v", err)
		}
		if n != 1 {
			t.Errorf("expected 1 deleted, got %d", n)
		}
		ids, err := client.PQGetIDs(ctx)
		if err != nil {
			t.Fatalf("PQGetIDs: %v", err)
		}
		if len(ids) != 0 {
			t.Errorf("expected empty queue, got %v", ids)
		}
	})
}

func TestPostgresExchangeEvents(t *testing.T) {
	t.Run("consumer attaching after produce still receives the event", func(t *testing.T) {
		client := newExchangeTestClient(t)
		ctx := context.Background()

		// Produce BEFORE any consumer exists — the durable-until-consumed
		// semantic bare LISTEN/NOTIFY would silently break.
		sent, err := client.ECProducerSendEvents(ctx, []dbapi.BatchEvent{
			{ID: "late-job", Type: dbapi.BatchEventCancel, TTL: 3600},
		})
		if err != nil {
			t.Fatalf("ECProducerSendEvents: %v", err)
		}
		if len(sent) != 1 {
			t.Fatalf("expected 1 sent event, got %d", len(sent))
		}

		ch, err := client.ECConsumerGetChannel(ctx, "late-job")
		if err != nil {
			t.Fatalf("ECConsumerGetChannel: %v", err)
		}
		defer ch.CloseFn()

		select {
		case ev := <-ch.Events:
			if ev.ID != "late-job" || ev.Type != dbapi.BatchEventCancel {
				t.Errorf("unexpected event: %+v", ev)
			}
		case <-time.After(3 * pollFallbackInterval):
			t.Fatal("late-attached consumer never received the pre-produced event")
		}
	})

	t.Run("live event is delivered to an attached consumer", func(t *testing.T) {
		client := newExchangeTestClient(t)
		ctx := context.Background()

		ch, err := client.ECConsumerGetChannel(ctx, "live-job")
		if err != nil {
			t.Fatalf("ECConsumerGetChannel: %v", err)
		}
		defer ch.CloseFn()

		if _, err := client.ECProducerSendEvents(ctx, []dbapi.BatchEvent{
			{ID: "live-job", Type: dbapi.BatchEventCancel, TTL: 3600},
		}); err != nil {
			t.Fatalf("ECProducerSendEvents: %v", err)
		}

		select {
		case ev := <-ch.Events:
			if ev.ID != "live-job" || ev.Type != dbapi.BatchEventCancel {
				t.Errorf("unexpected event: %+v", ev)
			}
		case <-time.After(3 * pollFallbackInterval):
			t.Fatal("attached consumer never received the event")
		}
	})

	t.Run("events for one job are not delivered to another job's consumer", func(t *testing.T) {
		client := newExchangeTestClient(t)
		ctx := context.Background()

		chA, err := client.ECConsumerGetChannel(ctx, "job-a")
		if err != nil {
			t.Fatalf("ECConsumerGetChannel(job-a): %v", err)
		}
		defer chA.CloseFn()

		if _, err := client.ECProducerSendEvents(ctx, []dbapi.BatchEvent{
			{ID: "job-b", Type: dbapi.BatchEventCancel, TTL: 3600},
		}); err != nil {
			t.Fatalf("ECProducerSendEvents: %v", err)
		}

		select {
		case ev := <-chA.Events:
			t.Errorf("job-a consumer received job-b's event: %+v", ev)
		case <-time.After(2 * pollFallbackInterval):
			// Correct: nothing delivered to the wrong consumer.
		}
	})
}

func TestPostgresExchangeStatus(t *testing.T) {
	t.Run("set get delete roundtrip", func(t *testing.T) {
		client := newExchangeTestClient(t)
		ctx := context.Background()

		payload := []byte(`{"total": 10, "completed": 4, "failed": 1}`)
		if err := client.StatusSet(ctx, "job-1", 3600, payload); err != nil {
			t.Fatalf("StatusSet: %v", err)
		}
		got, err := client.StatusGet(ctx, "job-1")
		if err != nil {
			t.Fatalf("StatusGet: %v", err)
		}
		if string(got) != string(payload) {
			t.Errorf("expected %s, got %s", payload, got)
		}

		n, err := client.StatusDelete(ctx, "job-1")
		if err != nil {
			t.Fatalf("StatusDelete: %v", err)
		}
		if n != 1 {
			t.Errorf("expected 1 deleted, got %d", n)
		}
		got, err = client.StatusGet(ctx, "job-1")
		if err != nil {
			t.Fatalf("StatusGet after delete: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil after delete, got %s", got)
		}
	})

	t.Run("expired status is invisible and reclaimed by sweep", func(t *testing.T) {
		client := newExchangeTestClient(t)
		ctx := context.Background()

		if err := client.StatusSet(ctx, "ephemeral", 1, []byte(`{"total": 1}`)); err != nil {
			t.Fatalf("StatusSet: %v", err)
		}
		time.Sleep(1200 * time.Millisecond)

		got, err := client.StatusGet(ctx, "ephemeral")
		if err != nil {
			t.Fatalf("StatusGet: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil for expired status, got %s", got)
		}

		if err := client.SweepExpired(ctx); err != nil {
			t.Fatalf("SweepExpired: %v", err)
		}
		got, err = client.StatusGet(ctx, "ephemeral")
		if err != nil {
			t.Fatalf("StatusGet after sweep: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil after sweep, got %s", got)
		}
	})
}

func TestPostgresExchangeInFlight(t *testing.T) {
	t.Run("set heartbeat getall delete roundtrip", func(t *testing.T) {
		client := newExchangeTestClient(t)
		ctx := context.Background()

		if err := client.InFlightSet(ctx, "job-1", "proc-a"); err != nil {
			t.Fatalf("InFlightSet(job-1): %v", err)
		}
		if err := client.InFlightSet(ctx, "job-2", "proc-b"); err != nil {
			t.Fatalf("InFlightSet(job-2): %v", err)
		}

		entries, err := client.InFlightGetAll(ctx)
		if err != nil {
			t.Fatalf("InFlightGetAll: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(entries))
		}
		if e := entries["job-1"]; e == nil || e.ProcessorID != "proc-a" {
			t.Errorf("job-1 entry wrong: %+v", entries["job-1"])
		}
		firstSeen := entries["job-1"].LastSeen

		// Heartbeat: re-Set must refresh last_seen, not error on conflict.
		time.Sleep(1100 * time.Millisecond)
		if err := client.InFlightSet(ctx, "job-1", "proc-a"); err != nil {
			t.Fatalf("InFlightSet heartbeat: %v", err)
		}
		entries, err = client.InFlightGetAll(ctx)
		if err != nil {
			t.Fatalf("InFlightGetAll after heartbeat: %v", err)
		}
		if entries["job-1"].LastSeen <= firstSeen {
			t.Errorf("heartbeat did not advance LastSeen: before=%d after=%d",
				firstSeen, entries["job-1"].LastSeen)
		}

		if err := client.InFlightDelete(ctx, "job-1"); err != nil {
			t.Fatalf("InFlightDelete: %v", err)
		}
		entries, err = client.InFlightGetAll(ctx)
		if err != nil {
			t.Fatalf("InFlightGetAll after delete: %v", err)
		}
		if len(entries) != 1 || entries["job-2"] == nil {
			t.Errorf("expected only job-2 to remain, got %+v", entries)
		}
	})
}
