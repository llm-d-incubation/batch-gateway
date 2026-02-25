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

func (c *BatchDSClientRedis) DBStore(ctx context.Context, item *db_api.BatchItem) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if err = item.Validate(); err != nil {
		logger.Error(err, "DBStore: item validation failed")
		return
	}
	logger = logger.WithValues("ID", item.ID)

	ptags, err := packTags(item.Tags)
	if err != nil {
		logger.Error(err, "DBStore: tags packing failed")
		return err
	}
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	res, err := redisScriptStore.Run(cctx, c.redisClient,
		[]string{getKeyForStore(item.ID, c.tableName)},
		versionV1, item.ID, item.TenantID, item.Expiry, ptags, item.Status, item.Spec, ttlSecDefault).Text()
	if err != nil {
		logger.Error(err, "DBStore: script failed")
		return err
	}
	if len(res) > 0 {
		err = fmt.Errorf("%s", res)
		logger.Error(err, "DBStore: script failed")
		return
	}

	logger.Info("DBStore: succeeded")
	return nil
}

func (c *BatchDSClientRedis) DBUpdate(ctx context.Context, item *db_api.BatchItem) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if err = item.Validate(); err != nil {
		logger.Error(err, "DBUpdate:")
		return
	}
	logger = logger.WithValues("ID", item.ID)

	// Only update non-empty dynamic fields.
	var fields []interface{}

	if len(item.Status) > 0 {
		fields = append(fields, fieldNameStatus, item.Status)
	}

	if len(item.Tags) > 0 {
		var ptags string
		ptags, err = packTags(item.Tags)
		if err != nil {
			logger.Error(err, "DBUpdate: tags packing failed")
			return
		}
		fields = append(fields, fieldNameTags, ptags)
	}

	if len(fields) == 0 {
		logger.Info("DBUpdate: nothing to update")
		return nil
	}

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	err = c.redisClient.HSet(cctx, getKeyForStore(item.ID, c.tableName), fields...).Err()
	if err != nil {
		logger.Error(err, "DBUpdate: HSet failed")
		return
	}

	updatedStatus, updatedTags := len(item.Status) > 0, len(item.Tags) > 0
	logger.Info("DBUpdate: succeeded", "updatedStatus", updatedStatus, "updatedTags", updatedTags)
	return nil
}

func (c *BatchDSClientRedis) DBDelete(ctx context.Context, IDs []string) (
	deletedIDs []string, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)

	// Delete the items.
	resMap := make(map[string]*goredis.IntCmd)
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	var cmds []goredis.Cmder
	cmds, err = c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
		for _, id := range IDs {
			res := pipe.HDel(cctx, getKeyForStore(id, c.tableName),
				fieldNameVersion, fieldNameID, fieldNameTenantID, fieldNameExpiry, fieldNameTags, fieldNameStatus, fieldNameSpec)
			resMap[id] = res
		}
		return nil
	})
	if err != nil {
		logger.Error(err, "DBDelete: Pipelined failed")
		return
	}
	for _, cmd := range cmds {
		if cmd.Err() != nil && cmd.Err() != goredis.Nil {
			err = cmd.Err()
			logger.Error(err, "DBDelete: Command inside pipeline failed")
			break
		}
	}
	deletedIDs = make([]string, 0, len(resMap))
	for id, res := range resMap {
		if res != nil && res.Err() == nil && res.Val() > 0 {
			deletedIDs = append(deletedIDs, id)
		}
	}

	logger.Info("DBDelete: succeeded", "nItems", len(deletedIDs), "IDs", deletedIDs)

	return
}

func (c *BatchDSClientRedis) DBGet(
	ctx context.Context, query *db_api.BatchQuery,
	includeStatic bool, start, limit int) (
	items []*db_api.BatchItem, cursor int, expectMore bool, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if query == nil {
		logger.Info("DBGet: empty query")
		return
	}

	if len(query.IDs) > 0 {

		// Get the item records.
		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		defer ccancel()
		var cmds []goredis.Cmder
		cmds, err = c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
			for _, id := range query.IDs {
				if includeStatic {
					pipe.HMGet(cctx, getKeyForStore(id, c.tableName),
						fieldNameID, fieldNameTenantID, fieldNameExpiry, fieldNameTags, fieldNameStatus, fieldNameSpec)
				} else {
					pipe.HMGet(cctx, getKeyForStore(id, c.tableName),
						fieldNameID, fieldNameTenantID, fieldNameExpiry, fieldNameTags, fieldNameStatus)
				}
			}
			return nil
		})
		if err != nil {
			logger.Error(err, "DBGet: Pipelined failed")
			return
		}

		// Process the items.
		items = make([]*db_api.BatchItem, 0, len(cmds))
		for i, cmd := range cmds {
			if cmd.Err() != nil {
				if cmd.Err() != goredis.Nil {
					logger.Error(cmd.Err(), "DBGet: HMGet failed", "requestedID", query.IDs[i])
				}
				continue
			}

			hgetRes, ok := cmd.(*goredis.SliceCmd)
			if !ok {
				err = fmt.Errorf("unexpected result type from HMGet: %T", cmd)
				logger.Error(err, "DBGet:", "requestedID", query.IDs[i])
				return nil, 0, false, err
			}

			var item *db_api.BatchItem
			item, err = batchItemFromHget(hgetRes.Val(), includeStatic, logger)
			if err != nil {
				return nil, 0, false, err
			}

			// Filter by tenant if specified.
			if item != nil && len(query.TenantID) > 0 && item.TenantID != query.TenantID {
				items = append(items, item)
			}
		}
		cursor = len(items)
		expectMore = false

	} else if len(query.TagSelectors) > 0 {

		cond, found := db_api.LogicalCondNames[query.TagsLogicalCond]
		if !found {
			err = fmt.Errorf("invalid logical condition value: %d", query.TagsLogicalCond)
			logger.Error(err, "DBGet:")
			return
		}
		var res []interface{}
		ctags := convertTags(query.TagSelectors)
		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		defer ccancel()
		res, err = redisScriptGetByTags.Run(cctx, c.redisClient,
			ctags, cond, strconv.FormatBool(includeStatic), getKeyPatternForStore(c.tableName), start, limit, query.TenantID).Slice()
		if err != nil {
			logger.Error(err, "DBGet: script failed")
			return
		}
		cursor, expectMore, items, err = processGetScriptResult(res, includeStatic, logger)
		if err != nil {
			logger.Error(err, "DBGet:")
			return
		}

	} else if query.Expired {

		var res []interface{}
		curTimestamp := time.Now().Unix()
		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		defer ccancel()
		res, err = redisScriptGetByExpiry.Run(cctx, c.redisClient,
			[]string{}, curTimestamp, strconv.FormatBool(includeStatic),
			getKeyPatternForStore(c.tableName), start, limit, query.TenantID).Slice()
		if err != nil {
			logger.Error(err, "DBGet: script failed")
			return
		}
		cursor, expectMore, items, err = processGetScriptResult(res, includeStatic, logger)
		if err != nil {
			logger.Error(err, "DBGet:")
			return
		}

	}

	logger.Info("DBGet: succeeded", "nItems", len(items))

	return
}

func processGetScriptResult(res []interface{}, includeStatic bool, logger klog.Logger) (
	cursor int, expectMore bool, items []*db_api.BatchItem, err error) {

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
		vals := resItem.([]interface{})
		var item *db_api.BatchItem
		item, err = batchItemFromHget(vals, includeStatic, logger)
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
// Field positions: [0]=id, [1]=tenantID, [2]=expiry, [3]=tags, [4]=status, [5]=spec (if includeStatic).
func batchItemFromHget(vals []interface{}, includeStatic bool, logger klog.Logger) (item *db_api.BatchItem, err error) {

	if (includeStatic && len(vals) != 6) || (!includeStatic && len(vals) != 5) {
		err = fmt.Errorf("unexpected result contents from HMGet: %v", vals)
		return
	}

	ID, ok := vals[0].(string)
	if !ok || len(ID) == 0 {
		err = fmt.Errorf("missing or invalid id field: %v", vals[0])
		return
	}

	tenantID, ok := vals[1].(string)
	if !ok {
		tenantID = ""
	}

	var expiry int64
	if expiryStr, ok := vals[2].(string); ok && len(expiryStr) > 0 {
		expiry, err = strconv.ParseInt(expiryStr, 10, 64)
		if err != nil {
			err = fmt.Errorf("invalid expiry field %q: %w", expiryStr, err)
			return
		}
	}

	tags, ok := vals[3].(string)
	if !ok {
		tags = ""
	}
	nTags, err := unpackTags(tags)
	if err != nil {
		logger.Error(err, "batchItemFromHget:")
		return item, err
	}

	// Store the serialized status part (already in []byte form).
	var status []byte
	if statusStr, ok := vals[4].(string); ok && len(statusStr) > 0 {
		status = []byte(statusStr)
	}

	// Store the serialized spec part only if requested.
	var spec []byte
	if includeStatic {
		if specStr, ok := vals[5].(string); ok && len(specStr) > 0 {
			spec = []byte(specStr)
		}
	}

	item = &db_api.BatchItem{
		BaseIndexes: db_api.BaseIndexes{
			ID:       ID,
			TenantID: tenantID,
			Expiry:   expiry,
			Tags:     nTags,
		},
		BaseContents: db_api.BaseContents{
			Spec:   spec,
			Status: status,
		},
	}

	return
}
