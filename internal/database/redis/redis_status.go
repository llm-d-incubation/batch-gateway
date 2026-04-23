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

// This file provides a redis status client implementation.

package redis

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	goredis "github.com/redis/go-redis/v9"
)

func (c *ExchangeDBClientRedis) StatusSet(ctx context.Context, ID string, TTL int, data []byte) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)
	if len(ID) == 0 {
		err = fmt.Errorf("empty ID")
		return
	}
	logger = logger.WithValues("ID", ID)
	if len(data) == 0 {
		err = fmt.Errorf("empty data")
		return
	}
	if TTL <= 0 {
		TTL = ttlSecDefault
	}

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	res := c.redisClient.Set(cctx, getKeyForStatus(ID), data, time.Duration(TTL)*time.Second)
	if res == nil {
		err = fmt.Errorf("nil redis command result")
		return
	}
	if err = res.Err(); err != nil {
		return
	}

	logger.Info("StatusSet: succeeded")

	return
}

func (c *ExchangeDBClientRedis) StatusGet(ctx context.Context, ID string) (data []byte, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)
	if len(ID) == 0 {
		err = fmt.Errorf("empty ID")
		return
	}
	logger = logger.WithValues("ID", ID)

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	res := c.redisClient.Get(cctx, getKeyForStatus(ID))
	if res == nil {
		err = fmt.Errorf("nil redis command result")
		return
	}
	if res.Err() == goredis.Nil {
		logger.Info("StatusGet: no status")
		return
	} else if err = res.Err(); err != nil {
		return
	}

	data = []byte(res.Val())
	logger.Info("StatusGet: succeeded", "len(data)", len(data))

	return
}

func (c *ExchangeDBClientRedis) StatusDelete(ctx context.Context, ID string) (nDeleted int, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)
	if len(ID) == 0 {
		err = fmt.Errorf("empty ID")
		return
	}
	logger = logger.WithValues("ID", ID)

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	res := c.redisClient.Del(cctx, getKeyForStatus(ID))
	if res == nil {
		err = fmt.Errorf("nil redis command result")
		return
	}
	if err = res.Err(); err != nil {
		return
	}
	nDeleted = int(res.Val())

	logger.Info("StatusDelete: succeeded", "nDeleted", nDeleted)

	return
}

func getKeyForStatus(key string) string {
	return statusKeysPrefix + key
}
