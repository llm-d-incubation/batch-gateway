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

func (c *BatchDBClientRedis) DBStore(ctx context.Context, item *db_api.BatchItem) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	if err = item.Validate(); err != nil {
		klog.FromContext(ctx).Error(err, "DBStore[Batch]: item validation failed")
		return
	}
	return c.dbStore(ctx, &item.BaseIndexes, &item.BaseContents,
		itemTypeBatch, "DBStore[Batch]", nil)
}

func (c *FileDBClientRedis) DBStore(ctx context.Context, item *db_api.FileItem) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	if err = item.Validate(); err != nil {
		klog.FromContext(ctx).Error(err, "DBStore[File]: item validation failed")
		return
	}
	return c.dbStore(ctx, &item.BaseIndexes, &item.BaseContents,
		itemTypeFile, "DBStore[File]", []any{item.Purpose})
}

func (c *DSClientRedis) dbStore(ctx context.Context,
	indexes *db_api.BaseIndexes, contents *db_api.BaseContents,
	itemType, logPref string, extraFields []any) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx).WithValues("ID", indexes.ID)

	ptags, err := packTags(indexes.Tags)
	if err != nil {
		logger.Error(err, logPref+": tags packing failed")
		return err
	}
	args := []any{itemType, versionV1, indexes.ID, indexes.TenantID,
		indexes.Expiry, ptags, contents.Status, contents.Spec, ttlSecDefault}
	if len(extraFields) > 0 {
		args = append(args, extraFields...)
	}

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	res, err := redisScriptStore.Run(cctx, c.redisClient,
		[]string{getKeyForStore(indexes.ID, itemType)}, args...).Text()
	if err != nil {
		logger.Error(err, logPref+": script failed")
		return err
	}
	if len(res) > 0 {
		err = fmt.Errorf("%s", res)
		logger.Error(err, logPref+": script failed")
		return
	}

	logger.Info(logPref + ": succeeded")
	return nil
}

func getUpdateFields(status []byte, tags db_api.Tags) (
	fields []any, updateStatus, updateTags bool, err error) {

	fields = make([]any, 0, 2)
	if len(status) > 0 {
		fields = append(fields, fieldNameStatus, status)
		updateStatus = true
	}
	if len(tags) > 0 {
		var ptags string
		ptags, err = packTags(tags)
		if err != nil {
			return
		}
		fields = append(fields, fieldNameTags, ptags)
		updateTags = true
	}
	return
}

func (c *BatchDBClientRedis) DBUpdate(ctx context.Context, item *db_api.BatchItem) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	if err = item.Validate(); err != nil {
		klog.FromContext(ctx).Error(err, "DBUpdate[Batch]: item validation failed")
		return
	}
	return c.dbUpdate(ctx, &item.BaseIndexes, &item.BaseContents, itemTypeBatch, "DBUpdate[Batch]")
}

func (c *FileDBClientRedis) DBUpdate(ctx context.Context, item *db_api.FileItem) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	if err = item.Validate(); err != nil {
		klog.FromContext(ctx).Error(err, "DBUpdate[File]: item validation failed")
		return
	}
	return c.dbUpdate(ctx, &item.BaseIndexes, &item.BaseContents, itemTypeFile, "DBUpdate[File]")
}

func (c *DSClientRedis) dbUpdate(ctx context.Context,
	indexes *db_api.BaseIndexes, contents *db_api.BaseContents,
	itemType, logPref string) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx).WithValues("ID", indexes.ID)

	fields, updatedStatus, updatedTags, err := getUpdateFields(contents.Status, indexes.Tags)
	if err != nil {
		logger.Error(err, logPref+": getUpdateFields failed")
		return err
	}
	if len(fields) == 0 {
		logger.Info(logPref + ": nothing to update")
		return nil
	}

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	err = c.redisClient.HSet(cctx, getKeyForStore(indexes.ID, itemType), fields...).Err()
	if err != nil {
		logger.Error(err, logPref+": HSet failed")
		return
	}

	logger.Info(logPref+": succeeded", "updatedStatus", updatedStatus, "updatedTags", updatedTags)
	return nil
}

func (c *BatchDBClientRedis) DBDelete(ctx context.Context, IDs []string) (
	deletedIDs []string, err error) {
	return c.dBDelete(ctx, IDs, itemTypeBatch, "DBDelete[Batch]")
}

func (c *FileDBClientRedis) DBDelete(ctx context.Context, IDs []string) (
	deletedIDs []string, err error) {
	return c.dBDelete(ctx, IDs, itemTypeFile, "DBDelete[File]")
}

func (c *DSClientRedis) dBDelete(ctx context.Context, IDs []string, itemType, logPref string) (
	deletedIDs []string, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)

	// Delete the items.
	resMap := make(map[string]*goredis.IntCmd)
	var cmds []goredis.Cmder
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	cmds, err = c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
		for _, id := range IDs {
			res := pipe.Del(cctx, getKeyForStore(id, itemType))
			resMap[id] = res
		}
		return nil
	})
	if err != nil {
		logger.Error(err, logPref+": Pipelined failed")
		return
	}
	for _, cmd := range cmds {
		if cmd.Err() != nil && cmd.Err() != goredis.Nil {
			err = cmd.Err()
			logger.Error(err, logPref+": Command inside pipeline failed")
			break
		}
	}
	deletedIDs = make([]string, 0, len(resMap))
	for id, res := range resMap {
		if res != nil && res.Err() == nil && res.Val() > 0 {
			deletedIDs = append(deletedIDs, id)
		}
	}

	logger.Info(logPref+": succeeded", "nItems", len(deletedIDs), "IDs", deletedIDs)
	return
}

func (c *DSClientRedis) dbGet(
	ctx context.Context, itemType, logPref string, start, limit int, includeStatic bool,
	IDs []string, tagSelectors db_api.Tags, tagsLogicalCond db_api.LogicalCond,
	expired bool, tenantID, purpose string) (res []any, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	includeSpec := strconv.FormatBool(includeStatic)

	if len(IDs) > 0 {

		keys := make([]string, 0, len(IDs))
		for _, id := range IDs {
			keys = append(keys, getKeyForStore(id, itemType))
		}
		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		defer ccancel()
		res, err = redisScriptGetByIDs.Run(cctx, c.redisClient,
			keys, tenantID, includeSpec).Slice()
		if err != nil {
			logger.Error(err, logPref+": script failed")
			return
		}

	} else if len(tagSelectors) > 0 {

		cond, found := db_api.LogicalCondNames[tagsLogicalCond]
		if !found {
			err = fmt.Errorf("invalid logical condition value: %d", tagsLogicalCond)
			logger.Error(err, logPref+":")
			return
		}
		ctags := convertTags(tagSelectors)
		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		defer ccancel()
		res, err = redisScriptGetByTags.Run(cctx, c.redisClient,
			ctags, cond, getKeyPatternForStore(itemType), start, limit, tenantID, includeSpec).Slice()
		if err != nil {
			logger.Error(err, logPref+": script failed")
			return
		}

	} else if expired {

		curTimestamp := time.Now().Unix()
		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		defer ccancel()
		res, err = redisScriptGetByExpiry.Run(cctx, c.redisClient,
			[]string{}, curTimestamp, getKeyPatternForStore(itemType),
			start, limit, tenantID, includeSpec).Slice()
		if err != nil {
			logger.Error(err, logPref+": script failed")
			return
		}

	} else if len(purpose) > 0 {

		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		defer ccancel()
		res, err = redisScriptGetByPurpose.Run(cctx, c.redisClient,
			[]string{}, purpose, getKeyPatternForStore(itemType),
			start, limit, tenantID, includeSpec).Slice()
		if err != nil {
			logger.Error(err, logPref+": script failed")
			return
		}

	} else if len(tenantID) > 0 {

		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		defer ccancel()
		res, err = redisScriptGetByTenant.Run(cctx, c.redisClient,
			[]string{}, tenantID, getKeyPatternForStore(itemType),
			start, limit, includeSpec).Slice()
		if err != nil {
			logger.Error(err, logPref+": script failed")
			return
		}

	}

	return
}

func (c *BatchDBClientRedis) DBGet(
	ctx context.Context, query *db_api.BatchQuery,
	includeStatic bool, start, limit int) (
	items []*db_api.BatchItem, cursor int, expectMore bool, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if query == nil {
		logger.Info("DBGet[Batch]: empty query")
		return
	}

	var res []any
	res, err = c.dbGet(ctx, itemTypeBatch, "DBGet[Batch]", start, limit, includeStatic,
		query.IDs, query.TagSelectors, query.TagsLogicalCond, query.Expired, query.TenantID, "")
	if err != nil {
		return
	}
	if res != nil {
		cursor, expectMore, items, err = processGetScriptResultBatch(res)
		if err != nil {
			logger.Error(err, "DBGet[Batch]: processGetScriptResultBatch failed")
			return
		}
	}

	logger.Info("DBGet[Batch]: succeeded", "nItems", len(items))
	return
}

func (c *FileDBClientRedis) DBGet(
	ctx context.Context, query *db_api.FileQuery,
	includeStatic bool, start, limit int) (
	items []*db_api.FileItem, cursor int, expectMore bool, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if query == nil {
		logger.Info("DBGet[File]: empty query")
		return
	}

	var res []any
	res, err = c.dbGet(ctx, itemTypeFile, "DBGet[File]", start, limit, includeStatic,
		query.IDs, query.TagSelectors, query.TagsLogicalCond, query.Expired, query.TenantID, query.Purpose)
	if err != nil {
		return
	}
	if res != nil {
		cursor, expectMore, items, err = processGetScriptResultFile(res)
		if err != nil {
			logger.Error(err, "DBGet[File]: processGetScriptResultFile failed")
			return
		}
	}

	logger.Info("DBGet[File]: succeeded", "nItems", len(items))
	return
}

func processGetScriptResultBatch(res []any) (
	cursor int, expectMore bool, items []*db_api.BatchItem, err error) {

	if len(res) != 2 {
		err = fmt.Errorf("unexpected result from script")
		return
	}
	resItems, ok := res[1].([]any)
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
		item, err := batchItemFromHget(resItem.([]any))
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

func processGetScriptResultFile(res []any) (
	cursor int, expectMore bool, items []*db_api.FileItem, err error) {

	if len(res) != 2 {
		err = fmt.Errorf("unexpected result from script")
		return
	}
	resItems, ok := res[1].([]any)
	if !ok {
		err = fmt.Errorf("unexpected result type from script: %T", res[1])
		return
	}
	resCursor, ok := res[0].(int64)
	if !ok {
		err = fmt.Errorf("unexpected result type from script: %T", res[0])
		return
	}
	items = make([]*db_api.FileItem, 0, len(resItems))
	for _, resItem := range resItems {
		item, err := fileItemFromHget(resItem.([]any))
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

func getKeyForStore(key, itemType string) string {
	return storeKeysPrefix + itemType + ":" + key
}

func getKeyPatternForStore(itemType string) string {
	return storeKeysPrefix + itemType + ":*"
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

func batchItemFromHget(vals []any) (item *db_api.BatchItem, err error) {

	ID, tenantID, expiry, tags, _, status, spec, err := itemFromHget(vals)
	if err != nil {
		return nil, err
	}

	item = &db_api.BatchItem{
		BaseIndexes: db_api.BaseIndexes{
			ID:       ID,
			TenantID: tenantID,
			Expiry:   expiry,
			Tags:     tags,
		},
		BaseContents: db_api.BaseContents{
			Spec:   spec,
			Status: status,
		},
	}

	return
}

func fileItemFromHget(vals []any) (item *db_api.FileItem, err error) {

	ID, tenantID, expiry, tags, purpose, status, spec, err := itemFromHget(vals)
	if err != nil {
		return nil, err
	}

	item = &db_api.FileItem{
		BaseIndexes: db_api.BaseIndexes{
			ID:       ID,
			TenantID: tenantID,
			Expiry:   expiry,
			Tags:     tags,
		},
		Purpose: purpose,
		BaseContents: db_api.BaseContents{
			Spec:   spec,
			Status: status,
		},
	}

	return
}

func itemFromHget(vals []any) (
	ID, tenantID string, expiry int64, tags db_api.Tags,
	purpose string, status, spec []byte, err error) {

	if len(vals)%2 != 0 {
		err = fmt.Errorf("unexpected result contents from HGETALL (odd length): %v", vals)
		return
	}

	// HGETALL returns a flat array: [field1, value1, field2, value2, ...].
	// Build a map from the flat array.
	hash := make(map[string]string)
	for i := 0; i < len(vals); i += 2 {
		fieldName, ok := vals[i].(string)
		if !ok {
			err = fmt.Errorf("invalid field name at index %d: %v", i, vals[i])
			return
		}
		fieldValue, ok := vals[i+1].(string)
		if !ok {
			err = fmt.Errorf("invalid field value at index %d: %v", i+1, vals[i+1])
			return
		}
		hash[fieldName] = fieldValue
	}

	// Extract ID (required).
	ID = hash["ID"]
	if len(ID) == 0 {
		err = fmt.Errorf("missing or invalid id field")
		return
	}

	// Extract tenantID.
	tenantID = hash["tenantID"]

	// Extract expiry.
	if expiryStr := hash["expiry"]; len(expiryStr) > 0 {
		expiry, err = strconv.ParseInt(expiryStr, 10, 64)
		if err != nil {
			err = fmt.Errorf("invalid expiry field %q: %w", expiryStr, err)
			return
		}
	}

	// Extract tags.
	tagsStr := hash["tags"]
	tags, err = unpackTags(tagsStr)
	if err != nil {
		return
	}

	// Extract purpose.
	purpose = hash["purpose"]

	// Extract status.
	if statusStr := hash["status"]; len(statusStr) > 0 {
		status = []byte(statusStr)
	}

	// Extract spec.
	if specStr := hash["spec"]; len(specStr) > 0 {
		spec = []byte(specStr)
	}

	return
}
