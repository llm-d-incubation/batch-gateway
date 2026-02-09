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

func (c *BatchDSClientRedis) DBStore(ctx context.Context, item *db_api.BatchItem) (
	ID string, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if item == nil {
		err = fmt.Errorf("item is empty")
		logger.Error(err, "DBStore:")
		return
	}
	if err = item.IsValid(); err != nil {
		logger.Error(err, "DBStore: item is invalid")
		return
	}
	logger = logger.WithValues("ID", item.ID)

	// Store the item.
	ptags, err := packTags(item.Tags)
	if err != nil {
		logger.Error(err, "DBStore: tags packing failed")
		return "", err
	}
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	res, err := redisScriptStore.Run(cctx, c.redisClient,
		[]string{getKeyForStore(item.ID, c.tableName)},
		versionV1, item.ID, item.Expiry,
		ptags, item.Status, item.Spec,
		ttlSecDefault).Text()
	if err != nil {
		logger.Error(err, "DBStore: script failed")
		return "", err
	}
	if len(res) > 0 {
		err = fmt.Errorf("%s", res)
		logger.Error(err, "DBStore: script failed")
		return
	}

	logger.Info("DBStore: succeeded")

	return item.ID, nil
}

func (c *BatchDSClientRedis) DBUpdate(ctx context.Context, item *db_api.BatchItem) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if item == nil || len(item.ID) == 0 {
		err = fmt.Errorf("item is empty or invalid")
		logger.Error(err, "DBUpdate:")
		return
	}
	logger = logger.WithValues("ID", item.ID)
	if len(item.Status) == 0 && len(item.Tags) == 0 {
		logger.Info("DBUpdate: nothing to update")
		return
	}

	// Update the item in the database.
	ptags, err := packTags(item.Tags)
	if err != nil {
		logger.Error(err, "DBUpdate: tags packing failed")
		return err
	}
	key := getKeyForStore(item.ID, c.tableName)
	updatedStatus, updatedTags := false, false
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	switch {
	case len(item.Status) > 0 && len(item.Tags) > 0:
		err = c.redisClient.HSet(cctx, key, fieldNameStatus, item.Status, fieldNameTags, ptags).Err()
		updatedStatus, updatedTags = true, true
	case len(item.Status) > 0:
		err = c.redisClient.HSet(cctx, key, fieldNameStatus, item.Status).Err()
		updatedStatus = true
	case len(item.Tags) > 0:
		err = c.redisClient.HSet(cctx, key, fieldNameTags, ptags).Err()
		updatedTags = true
	}
	if err != nil {
		logger.Error(err, "DBUpdate: redis HSet failed")
		return err
	}

	logger.Info("DBUpdate: succeeded", "updatedStatus", updatedStatus, "updatedTags", updatedTags)

	return
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
	cmds, err := c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
		for _, id := range IDs {
			res := pipe.HDel(cctx, getKeyForStore(id, c.tableName),
				fieldNameVersion, fieldNameId, fieldNameExpiry, fieldNameTags, fieldNameStatus, fieldNameSpec)
			resMap[id] = res
		}
		return nil
	})
	if err != nil {
		logger.Error(err, "DBDelete: Pipelined failed")
		return nil, err
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
	ctx context.Context, query *db_api.BatchDBQuery,
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
			logger.Error(err, "DBGet: Pipelined failed")
			return nil, 0, false, err
		}

		// Process the items.
		items = make([]*db_api.BatchItem, 0, len(cmds))
		for _, cmd := range cmds {
			if cmd.Err() != nil {
				if cmd.Err() != goredis.Nil {
					logger.Error(cmd.Err(), "DBGet: HMGet failed")
				}
				continue
			}
			hgetRes, ok := cmd.(*goredis.SliceCmd)
			if !ok {
				err := fmt.Errorf("unexpected result type from HMGet: %T", cmd)
				logger.Error(err, "DBGet:")
				return nil, 0, false, err
			}
			item, err := dbItemFromHget(hgetRes.Val(), includeStatic, logger)
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

		cond, found := db_api.GenLogicalCondNames[query.TagsLogicalCond]
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
			ctags, strconv.FormatBool(includeStatic), getKeyPatternForStore(c.tableName), cond, start, limit).Slice()
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
		res, err = redisScriptGetByExpiry.Run(cctx, c.redisClient,
			[]string{}, curTimestamp, getKeyPatternForStore(c.tableName),
			strconv.FormatBool(includeStatic), start, limit).Slice()
		defer ccancel()
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
		item, err := dbItemFromHget(resItem.([]interface{}), includeStatic, logger)
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

func dbItemFromHget(vals []interface{}, includeStatic bool, logger klog.Logger) (item *db_api.BatchItem, err error) {

	if (includeStatic && len(vals) != 5) || (!includeStatic && len(vals) != 4) {
		err = fmt.Errorf("unexpected result contents from HMGet: %v", vals)
		logger.Error(err, "dbItemFromHget:")
		return
	}
	var (
		id, tagsStr, status, spec string
		expiry                    int64
		tags                      db_api.Tags
		ok                        bool
	)
	id, ok = vals[0].(string)
	if !ok || len(id) == 0 {
		err = fmt.Errorf("invalid id: %v", id)
		logger.Error(err, "dbItemFromHget:")
		return
	}
	expiry, ok = vals[1].(int64)
	if !ok {
		expiry = 0
	}
	tagsStr, ok = vals[2].(string)
	if ok {
		tags, err = unpackTags(tagsStr)
		if err != nil {
			logger.Error(err, "dbItemFromHget: unpackTags failed")
			return
		}
	}
	// slo, ok = vals[1].(string) TBR
	// if ok && len(slo) > 0 {
	// 	sloNano, err := strconv.ParseInt(slo, 10, 64)
	// 	if err != nil {
	// 		logger.Error(err, "dbItemFromHget:")
	// 		return nil, err
	// 	}
	// 	sloTime = time.Unix(0, sloNano)
	// }
	status, ok = vals[3].(string)
	if !ok {
		status = ""
	}
	if includeStatic {
		spec, ok = vals[4].(string)
		if !ok {
			spec = ""
		}
	}
	item = &db_api.BatchItem{
		ID:     id,
		Expiry: expiry,
		Tags:   tags,
		Spec:   []byte(spec),
		Status: []byte(status),
	}
	return
}
