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
	"sync"
	"time"

	"github.com/go-logr/logr"
	db_api "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	goredis "github.com/redis/go-redis/v9"
)

const (
	fieldNameVersion     = "ver"
	fieldNameID          = "ID"
	fieldNameTenantID    = "tenantID"
	fieldNameExpiry      = "expiry"
	fieldNameTags        = "tags"
	fieldNameSpec        = "spec"
	fieldNameStatus      = "status"
	fieldNamePurpose     = "purpose"
	itemTypeBatch        = "batch"
	itemTypeFile         = "file"
	keysPrefix           = "llmd_batch:"
	storeKeysPrefix      = keysPrefix + "store:"
	queueKeysPrefix      = keysPrefix + "queue:"
	eventKeysPrefix      = keysPrefix + "event:"
	statusKeysPrefix     = keysPrefix + "status:"
	priorityQueueKeyName = queueKeysPrefix + "priority"
	eventChanSize        = 100
	eventReadCount       = 4
	eventReadTimeout     = 20 * time.Second
	eventChanTimeout     = 20 * time.Second
	cmdTimeout           = 20 * time.Second
	routineStopTimeout   = 20 * time.Second
	ttlSecDefault        = 60 * 60 * 24 * 60
	logFreqDefault       = 10 * time.Minute
	versionV1            = "1"
)

var (
	//go:embed redis_common.lua
	commonLua string

	//go:embed redis_store.lua
	storeLua         string
	redisScriptStore = goredis.NewScript(storeLua)

	//go:embed redis_get_by_ids.lua
	getByIDsLua         string
	redisScriptGetByIDs = goredis.NewScript(commonLua + "\n" + getByIDsLua)

	//go:embed redis_get_by_tags.lua
	getByTagsLua         string
	redisScriptGetByTags = goredis.NewScript(commonLua + "\n" + getByTagsLua)

	//go:embed redis_get_by_expiry.lua
	getByExpiryLua         string
	redisScriptGetByExpiry = goredis.NewScript(commonLua + "\n" + getByExpiryLua)

	//go:embed redis_get_by_purpose.lua
	getByPurposeLua         string
	redisScriptGetByPurpose = goredis.NewScript(commonLua + "\n" + getByPurposeLua)

	//go:embed redis_get_by_tenant.lua
	getByTenantLua         string
	redisScriptGetByTenant = goredis.NewScript(commonLua + "\n" + getByTenantLua)

	_ db_api.BatchDBClient            = (*BatchDBClientRedis)(nil)
	_ db_api.FileDBClient             = (*FileDBClientRedis)(nil)
	_ db_api.BatchPriorityQueueClient = (*ExchangeDBClientRedis)(nil)
	_ db_api.BatchEventChannelClient  = (*ExchangeDBClientRedis)(nil)
	_ db_api.BatchStatusClient        = (*ExchangeDBClientRedis)(nil)
)

type DSClientRedis struct {
	redisClient        *goredis.Client
	redisClientChecker *uredis.RedisClientChecker
	timeout            time.Duration
	idleLogFreq        time.Duration
	idleLogLast        time.Time
	onceClose          *sync.Once
}

type BatchDBClientRedis struct {
	*DSClientRedis
}

type FileDBClientRedis struct {
	*DSClientRedis
}

type ExchangeDBClientRedis struct {
	*DSClientRedis
}

// NewBatchDBClientRedis returns a new redis based batch db client.
// Provide either an already created baseRedisClient (that can be shared between multiple higher level redis based clients),
// or a conf and opTimeout for creating a new base redis client dedicated to this higher level client.
func NewBatchDBClientRedis(ctx context.Context, baseRedisClient *DSClientRedis, conf *uredis.RedisClientConfig, opTimeout time.Duration) (
	redisClient *BatchDBClientRedis, err error) {

	if baseRedisClient == nil {
		baseRedisClient, err = NewDSClientRedis(ctx, conf, opTimeout)
		if err != nil {
			return nil, err
		}
	}
	redisClient = &BatchDBClientRedis{DSClientRedis: baseRedisClient}
	return
}

// NewFilesDBClientRedis returns a new redis based file db client.
// Provide either an already created baseRedisClient (that can be shared between multiple higher level redis based clients),
// or a conf and opTimeout for creating a new base redis client dedicated to this higher level client.
func NewFileDBClientRedis(ctx context.Context, baseRedisClient *DSClientRedis, conf *uredis.RedisClientConfig, opTimeout time.Duration) (
	redisClient *FileDBClientRedis, err error) {

	if baseRedisClient == nil {
		baseRedisClient, err = NewDSClientRedis(ctx, conf, opTimeout)
		if err != nil {
			return nil, err
		}
	}
	redisClient = &FileDBClientRedis{DSClientRedis: baseRedisClient}
	return
}

// NewExchangeDBClientRedis returns a new redis based exchange db client.
// Provide either an already created baseRedisClient (that can be shared between multiple higher level redis based clients),
// or a conf and opTimeout for creating a new base redis client dedicated to this higher level client.
func NewExchangeDBClientRedis(ctx context.Context, baseRedisClient *DSClientRedis, conf *uredis.RedisClientConfig, opTimeout time.Duration) (
	redisClient *ExchangeDBClientRedis, err error) {

	if baseRedisClient == nil {
		baseRedisClient, err = NewDSClientRedis(ctx, conf, opTimeout)
		if err != nil {
			return nil, err
		}
	}
	redisClient = &ExchangeDBClientRedis{DSClientRedis: baseRedisClient}
	return
}

func NewDSClientRedis(ctx context.Context, conf *uredis.RedisClientConfig, opTimeout time.Duration) (
	*DSClientRedis, error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)
	if conf == nil {
		err := fmt.Errorf("empty redis config")
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
	logger.Info("NewDSClientRedis: succeeded", "serviceName", conf.ServiceName)
	return &DSClientRedis{
		redisClient:        redisClient,
		redisClientChecker: redisClientChecker,
		timeout:            opTimeout,
		idleLogFreq:        logFreqDefault,
		idleLogLast:        time.Now(),
		onceClose:          &sync.Once{},
	}, nil
}

func (c *DSClientRedis) Close() (err error) {
	c.onceClose.Do(func() {
		if c.redisClient != nil {
			err = c.redisClient.Close()
		}
	})
	return
}
