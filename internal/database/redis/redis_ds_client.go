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

// This file provides a redis data structures client implementation.

package redis

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	db_api "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	goredis "github.com/redis/go-redis/v9"
	"k8s.io/klog/v2"
)

const (
	fieldNameVersion     = "ver"
	fieldNameId          = "id"
	fieldNameTenantID    = "tenantID"
	fieldNameExpiry      = "expiry"
	fieldNameSpec        = "spec"
	fieldNameStatus      = "status"
	fieldNameTags        = "tags"
	eventReadCount       = 4
	keysPrefix           = "llmd_batch:"
	storeKeysPrefix      = keysPrefix + "store:"
	queueKeysPrefix      = keysPrefix + "queue:"
	eventKeysPrefix      = keysPrefix + "event:"
	statusKeysPrefix     = keysPrefix + "status:"
	priorityQueueKeyName = queueKeysPrefix + "priority"
	routineStopTimeout   = 20 * time.Second
	eventChanTimeout     = 10 * time.Second
	cmdTimeout           = 20 * time.Second
	ttlSecDefault        = 60 * 60 * 24 * 60
	eventChanSize        = 100
	logFreqDefault       = 10 * time.Minute
	versionV1            = "1"
)

var (
	//go:embed redis_store.lua
	storeLua         string
	redisScriptStore = goredis.NewScript(storeLua)

	//go:embed redis_get_by_tags.lua
	getByTagsLua         string
	redisScriptGetByTags = goredis.NewScript(getByTagsLua)

	//go:embed redis_get_by_expiry.lua
	getByExpiryLua         string
	redisScriptGetByExpiry = goredis.NewScript(getByExpiryLua)

	_ db_api.BatchDBClient            = (*BatchDSClientRedis)(nil)
	_ db_api.BatchPriorityQueueClient = (*BatchDSClientRedis)(nil)
	_ db_api.BatchEventChannelClient  = (*BatchDSClientRedis)(nil)
	_ db_api.BatchStatusClient        = (*BatchDSClientRedis)(nil)
)

type BatchDSClientRedis struct {
	redisClient        *goredis.Client
	redisClientChecker *uredis.RedisClientChecker
	tableName          string
	timeout            time.Duration
	idleLogFreq        time.Duration
	idleLogLast        time.Time
}

func NewBatchDSClientRedis(ctx context.Context, conf *uredis.RedisClientConfig, opTimeout time.Duration, tableName string) (
	*BatchDSClientRedis, error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if conf == nil {
		err := fmt.Errorf("empty redis config")
		logger.Error(err, "NewBatchDSClientRedis:")
		return nil, err
	}
	if opTimeout <= 0 {
		opTimeout = cmdTimeout
	}
	redisClient, err := uredis.NewRedisClient(ctx, conf)
	if err != nil {
		return nil, err
	}
	redisClientChecker := uredis.NewRedisClientChecker(redisClient, keysPrefix, conf.ServiceName, opTimeout)
	logger.Info("NewBatchDSClientRedis: succeeded", "serviceName", conf.ServiceName)
	return &BatchDSClientRedis{
		redisClient:        redisClient,
		redisClientChecker: redisClientChecker,
		tableName:          tableName,
		timeout:            opTimeout,
		idleLogFreq:        logFreqDefault,
		idleLogLast:        time.Now(),
	}, nil
}

func (c *BatchDSClientRedis) Close() (err error) {
	if c.redisClient != nil {
		err = c.redisClient.Close()
	}
	return err
}

func (c *BatchDSClientRedis) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	if timeLimit > 0 {
		return context.WithTimeout(parentCtx, timeLimit)
	}
	return context.WithTimeout(parentCtx, c.timeout)
}
