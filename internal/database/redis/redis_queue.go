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
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	db_api "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	goredis "github.com/redis/go-redis/v9"
)

func (c *ExchangeDBClientRedis) PQEnqueue(ctx context.Context, item *db_api.BatchJobPriority) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)
	if item == nil {
		err = fmt.Errorf("empty item")
		logger.Error(err, "PQEnqueue:")
		return
	}
	if err = item.IsValid(); err != nil {
		logger.Error(err, "PQEnqueue: item is invalid")
		return
	}
	logger = logger.WithValues("ID", item.ID)

	data, lerr := json.Marshal(item)
	if lerr != nil {
		err = lerr
		logger.Error(err, "PQEnqueue: Marshal failed")
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
		logger.Error(err, "PQEnqueue:")
	}
	if cmdRes == nil {
		err = fmt.Errorf("redis command result is nil")
		logger.Error(err, "PQEnqueue:")
		return
	}
	for _, cmd := range cmdRes {
		if err = cmd.Err(); err != nil {
			logger.Error(err, "PQEnqueue: redis command failed", "cmd", cmd.Name())
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
		logger.Error(err, "PQDelete:")
		return
	}
	if err = item.IsValid(); err != nil {
		logger.Error(err, "PQDelete: item is invalid")
		return
	}
	logger = logger.WithValues("ID", item.ID)

	score := strconv.FormatInt(item.SLO.UnixMicro(), 10)
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	res := c.redisClient.ZRemRangeByScore(cctx, priorityQueueKeyName, score, score)
	if res == nil {
		err = fmt.Errorf("redis command result is nil")
		logger.Error(err, "PQDelete:")
		return
	}
	if res.Err() == goredis.Nil {
		logger.Info("PQDelete: key not found")
		return
	}
	if err = res.Err(); err != nil {
		logger.Error(err, "PQDelete: redis ZRemRangeByScore failed")
		return
	}
	nDeleted = int(res.Val())

	logger.Info("PQDelete: succeeded")
	return
}

func (c *ExchangeDBClientRedis) PQDequeue(ctx context.Context, timeout time.Duration, maxItems int) (
	jobPriorities []*db_api.BatchJobPriority, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)

	// Get items from the queue.
	// Use non-blocking ZMPop when timeout is zero, blocking BZMPop otherwise.
	var vals []goredis.Z
	if timeout <= 0 {
		logger.V(logging.DEBUG).Info("PQDequeue: Start ZMPop (non-blocking)")
		_, vals, err = c.redisClient.ZMPop(
			ctx, goredis.Min.String(), int64(maxItems), priorityQueueKeyName).Result()
		logger.V(logging.DEBUG).Info("PQDequeue: End ZMPop")
	} else {
		logger.V(logging.DEBUG).Info("PQDequeue: Start BZMPop", "timeout", timeout)
		_, vals, err = c.redisClient.BZMPop(
			ctx, timeout, goredis.Min.String(), int64(maxItems), priorityQueueKeyName).Result()
		logger.V(logging.DEBUG).Info("PQDequeue: End BZMPop")
	}
	if err != nil {
		if unrecognizedBlockingError(err) {
			logger.Error(err, "PQDequeue: B/ZMPop failed")
			cerr := c.redisClientChecker.Check(ctx)
			if cerr != nil {
				logger.Error(err, "PQDequeue: ClientCheck failed")
			}
			return nil, err
		}
		if time.Since(c.idleLogLast) >= c.idleLogFreq {
			logger.Info("PQDequeue: no items")
			c.idleLogLast = time.Now()
		}
		return nil, nil
	}
	if len(vals) == 0 {
		if time.Since(c.idleLogLast) >= c.idleLogFreq {
			logger.Info("PQDequeue: no items")
			c.idleLogLast = time.Now()
		}
		return nil, nil
	}

	jobPriorities = make([]*db_api.BatchJobPriority, 0, len(vals))
	for _, val := range vals {
		item := &db_api.BatchJobPriority{}
		err = json.Unmarshal([]byte(val.Member.(string)), item)
		if err != nil {
			logger.Error(err, "PQDequeue: Unmarshal failed")
			return
		}
		jobPriorities = append(jobPriorities, item)
	}

	logger.Info("PQDequeue: succeeded", "nItems", len(jobPriorities))
	return
}

func unrecognizedBlockingError(err error) bool {
	errStr := err.Error()
	unrecognized :=
		err != goredis.Nil &&
			!strings.Contains(errStr, "i/o timeout") &&
			!strings.Contains(errStr, "context")
	return unrecognized
}
