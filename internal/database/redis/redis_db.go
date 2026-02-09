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

// This file provides a redis database client implementation.

package redis

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	db_api "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	goredis "github.com/redis/go-redis/v9"
	"k8s.io/klog/v2"
)

func (c *BatchDSClientRedis) DBStore(ctx context.Context, item *db_api.BatchItem) error {
	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if err := db_api.IsBatchItemValid(item); err != nil {
		logger.Error(err, "Store:")
		return err
	}
	logger = logger.WithValues("ID", item.ID)

	// Serialize the static (spec) and dynamic (status) parts separately.
	specData, err := json.Marshal(item.Item.BatchSpec)
	if err != nil {
		logger.Error(err, "Store: spec serialization failed")
		return err
	}
	statusData, err := json.Marshal(item.Item.BatchStatusInfo)
	if err != nil {
		logger.Error(err, "Store: status serialization failed")
		return err
	}

	ptags, err := packTags(item.Tags)
	if err != nil {
		logger.Error(err, "Store: tags packing failed")
		return err
	}

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	res, err := redisScriptStore.Run(cctx, c.redisClient,
		[]string{getKeyForStore(item.ID, c.tableName)},
		versionV1, item.ID, item.Expiry, ptags, statusData, specData, ttlSecDefault).Text()
	if err != nil {
		logger.Error(err, "Store: script failed")
		return err
	}
	if len(res) > 0 {
		err = fmt.Errorf("%s", res)
		logger.Error(err, "Store: script failed")
		return err
	}

	logger.Info("Store: succeeded")
	return nil
}

func (c *BatchDSClientRedis) DBUpdate(ctx context.Context, item *db_api.BatchItem) error {
	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if err := db_api.IsBatchItemValid(item); err != nil {
		logger.Error(err, "Update:")
		return err
	}
	logger = logger.WithValues("ID", item.ID)

	// Serialize only the dynamic part (status).
	statusData, err := json.Marshal(item.Item.BatchStatusInfo)
	if err != nil {
		logger.Error(err, "Update: status serialization failed")
		return err
	}

	ptags, err := packTags(item.Tags)
	if err != nil {
		logger.Error(err, "Update: tags packing failed")
		return err
	}

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	err = c.redisClient.HSet(cctx, getKeyForStore(item.ID, c.tableName),
		fieldNameStatus, statusData, fieldNameTags, ptags).Err()
	if err != nil {
		logger.Error(err, "Update: HSet failed")
		return err
	}

	logger.Info("Update: succeeded")
	return nil
}

func (c *BatchDSClientRedis) DBDelete(ctx context.Context, IDs []string) (
	deletedIDs []string, err error,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)

	// Delete the items.
	resMap := make(map[string]*goredis.IntCmd)
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	cmds, err := c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
		for _, id := range IDs {
			res := pipe.HDel(cctx, getKeyForStore(id, c.tableName),
				fieldNameVersion, fieldNameId, fieldNameExpiry, fieldNameTags, fieldNameStatus, fieldNameSpec)
			resMap[id] = res
		}
		return nil
	})
	if err != nil {
		logger.Error(err, "Delete: Pipelined failed")
		return nil, err
	}
	for _, cmd := range cmds {
		if cmd.Err() != nil && cmd.Err() != goredis.Nil {
			err = cmd.Err()
			logger.Error(err, "Delete: Command inside pipeline failed")
			break
		}
	}
	deletedIDs = make([]string, 0, len(resMap))
	for id, res := range resMap {
		if res != nil && res.Err() == nil && res.Val() > 0 {
			deletedIDs = append(deletedIDs, id)
		}
	}

	logger.Info("Delete: succeeded", "nItems", len(deletedIDs), "IDs", deletedIDs)

	return
}

func (c *BatchDSClientRedis) DBGet(
	ctx context.Context, query *db_api.Query,
	includeStatic bool, start, limit int) (
	items []*db_api.BatchItem, cursor int, expectMore bool, err error,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if query == nil {
		logger.Info("Get: empty query")
		return
	}

	if len(query.IDs) > 0 {

		// Get the item records.
		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		defer ccancel()
		cmds, err := c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
			for _, id := range query.IDs {
				if includeStatic {
					pipe.HMGet(cctx, getKeyForStore(id, c.tableName),
						fieldNameId, fieldNameExpiry, fieldNameTags, fieldNameStatus, fieldNameSpec)
				} else {
					pipe.HMGet(cctx, getKeyForStore(id, c.tableName),
						fieldNameId, fieldNameExpiry, fieldNameTags, fieldNameStatus)
				}
			}
			return nil
		})
		if err != nil {
			logger.Error(err, "Get: Pipelined failed")
			return nil, 0, false, err
		}

		// Process the items.
		items = make([]*db_api.BatchItem, 0, len(cmds))
		for _, cmd := range cmds {
			if cmd.Err() != nil {
				if cmd.Err() != goredis.Nil {
					logger.Error(cmd.Err(), "Get: HMGet failed")
				}
				continue
			}
			hgetRes, ok := cmd.(*goredis.SliceCmd)
			if !ok {
				err := fmt.Errorf("unexpected result type from HMGet: %T", cmd)
				logger.Error(err, "Get:")
				return nil, 0, false, err
			}
			item, err := batchItemFromHget(hgetRes.Val(), includeStatic, logger)
			if err != nil {
				return nil, 0, false, err
			}
			if item != nil {
				items = append(items, item)
			}
		}
		cursor = len(items)
		expectMore = false

	} else if len(query.TagSelectors) > 0 {

		cond, found := db_api.LogicalCondNames[query.TagsLogicalCond]
		if !found {
			err = fmt.Errorf("invalid logical condition value: %d", query.TagsLogicalCond)
			logger.Error(err, "Get:")
			return
		}
		var res []interface{}
		ctags := convertTags(query.TagSelectors)
		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		defer ccancel()
		res, err = redisScriptGetByTags.Run(cctx, c.redisClient,
			ctags, strconv.FormatBool(includeStatic), getKeyPatternForStore(c.tableName), cond, start, limit).Slice()
		if err != nil {
			logger.Error(err, "Get: script failed")
			return
		}
		cursor, expectMore, items, err = processGetScriptResult(res, includeStatic, logger)
		if err != nil {
			logger.Error(err, "Get:")
			return
		}

	} else if query.Expired {

		var res []interface{}
		curTimestamp := time.Now().Unix()
		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		defer ccancel()
		res, err = redisScriptGetByExpiry.Run(cctx, c.redisClient,
			[]string{}, curTimestamp, getKeyPatternForStore(c.tableName),
			strconv.FormatBool(includeStatic), start, limit).Slice()
		if err != nil {
			logger.Error(err, "Get: script failed")
			return
		}
		cursor, expectMore, items, err = processGetScriptResult(res, includeStatic, logger)
		if err != nil {
			logger.Error(err, "Get:")
			return
		}

	}

	logger.Info("Get: succeeded", "nItems", len(items))

	return
}

func processGetScriptResult(res []interface{}, includeStatic bool, logger klog.Logger) (
	cursor int, expectMore bool, items []*db_api.BatchItem, err error,
) {
	if len(res) != 2 {
		err = fmt.Errorf("unexpected result from script")
		return
	}
	resItems, ok := res[1].([]interface{})
	if !ok {
		err = fmt.Errorf("unexpected result type from script: %T", res[1])
		return
	}
	resCursor, ok := res[0].(int64)
	if !ok {
		err = fmt.Errorf("unexpected result type from script: %T", res[0])
		return
	}
	items = make([]*db_api.BatchItem, 0, len(resItems))
	for _, resItem := range resItems {
		item, err := batchItemFromHget(resItem.([]interface{}), includeStatic, logger)
		if err != nil {
			return 0, false, nil, err
		}
		if item != nil {
			items = append(items, item)
		}
	}
	cursor = int(resCursor)
	expectMore = (cursor != 0)

	return
}

func getKeyForStore(key, tableName string) string {
	return storeKeysPrefix + tableName + ":" + key
}

func getKeyPatternForStore(tableName string) string {
	return storeKeysPrefix + tableName + ":*"
}

func packTags(tags map[string]string) (string, error) {
	if len(tags) == 0 {
		return "", nil
	}
	json, err := json.Marshal(tags)
	if err != nil {
		return "", err
	}
	return string(json), nil
}

func unpackTags(tagsPacked string) (map[string]string, error) {
	if len(tagsPacked) == 0 {
		return nil, nil
	}
	var tags map[string]string
	err := json.Unmarshal([]byte(tagsPacked), &tags)
	if err != nil {
		return nil, err
	}
	return tags, nil
}

func convertTags(tags map[string]string) (ctags []string) {
	if len(tags) > 0 {
		ctags = make([]string, 0, len(tags))
		for key, val := range tags {
			ctags = append(ctags, fmt.Sprintf("\"%s\":\"%s\"", key, val))
		}
	}
	return
}

// batchItemFromHget reconstructs a BatchItem from Redis HMGET results.
// Field positions: [0]=id, [1]=expiry, [2]=tags, [3]=status, [4]=spec (if includeStatic).
func batchItemFromHget(vals []interface{}, includeStatic bool, logger klog.Logger) (*db_api.BatchItem, error) {
	if (includeStatic && len(vals) != 5) || (!includeStatic && len(vals) != 4) {
		err := fmt.Errorf("unexpected result contents from HMGet: %v", vals)
		logger.Error(err, "batchItemFromHget:")
		return nil, err
	}

	id, ok := vals[0].(string)
	if !ok || len(id) == 0 {
		return nil, nil
	}

	tenantID, ok := vals[1].(string)
	if !ok {
		tenantID = ""
	}

	var expiry int64
	if expiryStr, ok := vals[2].(string); ok && len(expiryStr) > 0 {
		var err error

		expiry, err = strconv.ParseInt(expiryStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid expiry field %q: %w", expiryStr, err)
		}
	}

	tags, ok := vals[2].(string)
	if !ok {
		tags = ""
	}

	nTags, err := unpackTags(tags)
	if err != nil {
		logger.Error(err, "batchItemFromHget:")
		return nil, err
	}

	item := &db_api.BatchItem{
		ID:   id,
		Tags: nTags,
	}

	// Parse expiry from the hash field.
	if expiryStr, ok := vals[1].(string); ok && len(expiryStr) > 0 {
		if expiry, err := strconv.ParseInt(expiryStr, 10, 64); err == nil {
			item.Expiry = expiry
		}
	}

	// Deserialize the dynamic status part (always present).
	if statusStr, ok := vals[3].(string); ok && len(statusStr) > 0 {
		if err := json.Unmarshal([]byte(statusStr), &item.Item.BatchStatusInfo); err != nil {
			logger.Error(err, "batchItemFromHget: failed to unmarshal BatchStatusInfo")
			return nil, err
		}
	}

	// Deserialize the static spec part only if requested.
	if includeStatic {
		if specStr, ok := vals[4].(string); ok && len(specStr) > 0 {
			if err := json.Unmarshal([]byte(specStr), &item.Item.BatchSpec); err != nil {
				logger.Error(err, "batchItemFromHget: failed to unmarshal BatchSpec")
				return nil, err
			}
		}
	}

	return item, nil
}
