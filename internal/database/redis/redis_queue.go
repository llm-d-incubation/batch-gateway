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

// This file provides a redis priority queue client implementation.

package redis

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	goredis "github.com/redis/go-redis/v9"
)

// pqMember is the canonical representation used as the Redis sorted set member.
// Only identity fields are included so that all enqueue paths produce identical
// member bytes for the same logical job, regardless of optional metadata.
type pqMember struct {
	ID  string    `json:"id"`
	SLO time.Time `json:"slo"`
}

func (c *ExchangeDBClientRedis) PQEnqueue(ctx context.Context, item *db_api.BatchJobPriority) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)
	if item == nil {
		err = fmt.Errorf("empty item")
		return
	}
	if err = item.IsValid(); err != nil {
		return
	}
	logger = logger.WithValues("ID", item.ID)

	data, lerr := json.Marshal(pqMember{ID: item.ID, SLO: item.SLO.UTC().Truncate(time.Microsecond)})
	if lerr != nil {
		err = lerr
		return
	}
	zitem := goredis.Z{
		Score:  float64(item.SLO.UnixMicro()),
		Member: data,
	}
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	cmdRes, lerr := c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
		pipe.ZAddNX(cctx, priorityQueueKeyName, zitem)
		if item.TTL > 0 {
			pipe.Expire(cctx, priorityQueueKeyName, time.Duration(item.TTL)*time.Second)
		}
		return nil
	})
	if lerr != nil {
		err = lerr
	}
	if cmdRes == nil {
		err = fmt.Errorf("redis command result is nil")
		return
	}
	for _, cmd := range cmdRes {
		if err = cmd.Err(); err != nil {
			return
		}
	}

	logger.Info("PQEnqueue: succeeded")
	return
}

func (c *ExchangeDBClientRedis) PQDelete(ctx context.Context, item *db_api.BatchJobPriority) (nDeleted int, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)
	if item == nil {
		err = fmt.Errorf("empty item")
		return
	}
	if err = item.IsValid(); err != nil {
		return
	}
	logger = logger.WithValues("ID", item.ID)

	data, lerr := json.Marshal(pqMember{ID: item.ID, SLO: item.SLO.UTC().Truncate(time.Microsecond)})
	if lerr != nil {
		err = lerr
		return
	}
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	res := c.redisClient.ZRem(cctx, priorityQueueKeyName, data)
	if res == nil {
		err = fmt.Errorf("redis command result is nil")
		return
	}
	if res.Err() == goredis.Nil {
		logger.Info("PQDelete: key not found")
		return
	}
	if err = res.Err(); err != nil {
		return
	}
	nDeleted = int(res.Val())

	logger.Info("PQDelete: succeeded")
	return
}

func (c *ExchangeDBClientRedis) PQDequeueAndClaim(ctx context.Context, processorID string) (
	*db_api.BatchJobPriority, error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)
	if processorID == "" {
		return nil, fmt.Errorf("processorID is empty")
	}

	entry := db_api.InFlightEntry{
		ProcessorID: processorID,
		LastSeen:    time.Now().Unix(),
	}
	entryData, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal in-flight entry: %w", err)
	}

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	raw, err := redisScriptPQDequeueClaim.Run(cctx, c.redisClient,
		[]string{priorityQueueKeyName, inFlightKeyName}, entryData).Result()
	if err != nil {
		// The script returns false on an empty queue, which go-redis maps to Nil.
		if errors.Is(err, goredis.Nil) {
			if time.Since(c.idleLogLast) >= c.idleLogFreq {
				logger.Info("PQDequeueAndClaim: no items")
				c.idleLogLast = time.Now()
			}
			return nil, nil
		}
		// The polling loop treats dequeue errors as "no work" and keeps
		// spinning, so log here and (for connection-level failures) probe the
		// connection to surface Redis outages and read-only failovers that
		// would otherwise be silent.
		logger.Error(err, "PQDequeueAndClaim: script failed")
		// Server-side (script) errors already prove Redis is reachable; only
		// run the checker for connection-level failures.
		if _, ok := err.(goredis.RedisError); !ok && ctx.Err() == nil {
			checkCtx, cancel := context.WithTimeout(context.Background(), c.timeout)
			defer cancel()
			if cerr := c.redisClientChecker.Check(checkCtx); cerr != nil {
				logger.Error(cerr, "PQDequeueAndClaim: ClientCheck failed")
			}
		}
		return nil, fmt.Errorf("PQDequeueAndClaim: %w", err)
	}

	member, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("PQDequeueAndClaim: unexpected member type: %T", raw)
	}
	item := &db_api.BatchJobPriority{}
	if err := json.Unmarshal([]byte(member), item); err != nil {
		return nil, fmt.Errorf("PQDequeueAndClaim: unmarshal member: %w", err)
	}

	logger.Info("PQDequeueAndClaim: succeeded", "ID", item.ID)
	return item, nil
}

func (c *ExchangeDBClientRedis) PQGetIDs(ctx context.Context) (map[string]bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	raw, err := redisScriptPQGetIDs.Run(cctx, c.redisClient,
		[]string{priorityQueueKeyName}).StringSlice()
	if err != nil {
		return nil, fmt.Errorf("PQGetIDs: %w", err)
	}

	ids := make(map[string]bool, len(raw))
	for _, id := range raw {
		ids[id] = true
	}
	return ids, nil
}

func unrecognizedBlockingError(err error) bool {
	errStr := err.Error()
	unrecognized :=
		err != goredis.Nil &&
			!strings.Contains(errStr, "i/o timeout") &&
			!strings.Contains(errStr, "context")
	return unrecognized
}
