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

	db_api "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	goredis "github.com/redis/go-redis/v9"
	"k8s.io/klog/v2"
)

func (c *BatchDSClientRedis) PQEnqueue(ctx context.Context, item *db_api.BatchJobPriority) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
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
	res := c.redisClient.ZAddNX(cctx, priorityQueueKeyName, zitem)
	if res == nil {
		err = fmt.Errorf("redis command result is nil")
		logger.Error(err, "PQEnqueue:")
		return
	}
	if err = res.Err(); err != nil {
		logger.Error(err, "PQEnqueue: redis ZAddNX failed")
		return
	}

	logger.Info("PQEnqueue: succeeded")

	return
}

func (c *BatchDSClientRedis) PQDelete(ctx context.Context, item *db_api.BatchJobPriority) (nDeleted int, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
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
	if err = res.Err(); err != nil {
		logger.Error(err, "PQDelete: redis ZRemRangeByScore failed")
		return
	}
	nDeleted = int(res.Val())

	logger.Info("PQDelete: succeeded")

	return
}

func (c *BatchDSClientRedis) PQDequeue(ctx context.Context, timeout time.Duration, maxItems int) (
	jobPriorities []*db_api.BatchJobPriority, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)

	// Get items from the queue.
	cctx, ccancel := context.WithTimeout(ctx, timeout+2*time.Second)
	defer ccancel()
	_, vals, err := c.redisClient.BZMPop(
		cctx, timeout, goredis.Min.String(), int64(maxItems), priorityQueueKeyName).Result()
	if err != nil {
		if unrecognizedBlockingError(err) {
			logger.Error(err, "PQDequeue: BZMPop failed")
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
