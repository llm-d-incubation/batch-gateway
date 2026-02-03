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

// This file provides a redis database implementation.

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
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	goredis "github.com/redis/go-redis/v9"
	"k8s.io/klog/v2"
)

const (
	fieldNameVersion     = "ver"
	fieldNameId          = "id"
	fieldNameExpiry      = "expiry"
	fieldNameSpec        = "spec"
	fieldNameStatus      = "status"
	fieldNameTags        = "tags"
	eventReadCount       = 4
	keysPrefix           = "wx_batch:"
	storeKeysPrefix      = keysPrefix + "store:"
	queueKeysPrefix      = keysPrefix + "queue:"
	eventKeysPrefix      = keysPrefix + "event:"
	statusKeysPrefix     = keysPrefix + "status:"
	priorityQueueKeyName = queueKeysPrefix + "priority"
	storeKeysPattern     = storeKeysPrefix + "*"
	routineStopTimeout   = 20 * time.Second
	eventChanTimeout     = 10 * time.Second
	cmdTimeout           = 20 * time.Second
	ttlSecDefault        = 60 * 60 * 24 * 30
	tagsSep              = ";;"
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

	ptags, err := packTags(item.Tags)
	if err != nil {
		logger.Error(err, "DBStore: tags packing failed")
		return item.ID, err
	}
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	res, err := redisScriptStore.Run(cctx, c.redisClient,
		[]string{getKeyForStore(item.ID, c.tableName)},
		versionV1, item.ID, item.Expiry,
		ptags, item.Status, item.Spec,
		ttlSecDefault).Text()
	ccancel()
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
	updatedStatus, updatedTags := false, false
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	cmds, err := c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
		switch {
		case len(item.Status) > 0 && len(item.Tags) > 0:
			pipe.HSet(cctx, getKeyForStore(item.ID, c.tableName),
				fieldNameStatus, item.Status, fieldNameTags, ptags).Err()
			updatedStatus, updatedTags = true, true
		case len(item.Status) > 0:
			pipe.HSet(cctx, getKeyForStore(item.ID, c.tableName),
				fieldNameStatus, item.Status).Err()
			updatedStatus = true
		case len(item.Tags) > 0:
			pipe.HSet(cctx, getKeyForStore(item.ID, c.tableName),
				fieldNameTags, ptags).Err()
			updatedTags = true
		}
		return nil
	})
	ccancel()
	if err != nil {
		logger.Error(err, "DBUpdate: Pipelined failed")
		return err
	}
	for _, cmd := range cmds {
		if err = cmd.Err(); err != nil {
			logger.Error(err, "DBUpdate: Command inside pipeline failed")
			return
		}
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

	// Delete the item records.
	resMap := make(map[string]*goredis.IntCmd)
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	cmds, err := c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
		for _, id := range IDs {
			res := pipe.HDel(cctx, getKeyForStore(id, c.tableName),
				fieldNameVersion, fieldNameId, fieldNameExpiry, fieldNameTags, fieldNameStatus, fieldNameSpec)
			resMap[id] = res
		}
		return nil
	})
	ccancel()
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
	items []*db_api.BatchItem, cursor int, expectedMore bool, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)

	if len(query.IDs) > 0 {

		// Get the item records.
		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
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
		ccancel()
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
		res, err = redisScriptGetByTags.Run(cctx, c.redisClient,
			ctags, strconv.FormatBool(includeStatic), storeKeysPattern, cond, start, limit).Slice()
		ccancel()
		if err != nil {
			logger.Error(err, "DBGet: script failed")
			return
		}
		if len(res) != 2 {
			err = fmt.Errorf("unexpected result from script")
			logger.Error(err, "DBGet:")
			return
		}
		resItems, ok := res[1].([]interface{})
		if !ok {
			err = fmt.Errorf("unexpected result type from script: %T", res[1])
			logger.Error(err, "DBGet:")
			return
		}
		resCursor, ok := res[0].(int64)
		if !ok {
			err = fmt.Errorf("unexpected result type from script: %T", res[0])
			logger.Error(err, "DBGet:")
			return
		}
		items = make([]*db_api.BatchItem, 0, len(resItems))
		for _, resItem := range resItems {
			item, err := dbItemFromHget(resItem.([]interface{}), includeStatic, logger)
			if err != nil {
				return nil, 0, false, err
			}
			if item != nil {
				items = append(items, item)
			}
		}
		cursor = int(resCursor)
	}

	logger.Info("DBGet: succeeded", "nItems", len(items))

	return
}

func getKeyForStore(key, tableName string) string {
	return storeKeysPrefix + key + tableName + ":"
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
	// return fmt.Sprintf("%s%s%s", tagsSep, strings.Join(tags, tagsSep), tagsSep) TBR
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
	// rTags := strings.Split(tags, tagsSep) TBR
	// if len(rTags) > 2 {
	// 	return rTags[1 : len(rTags)-1]
	// } else {
	// 	return rTags
	// }
}

func convertTags(tags map[string]string) (ctags []string) {
	if len(tags) > 0 {
		ctags = make([]string, 0, len(tags))
		for key, val := range tags {
			ctags = append(ctags, fmt.Sprintf("\"%s\":\"%s\"", key, val))
			// ctags[i] = fmt.Sprintf("%s%s%s", tagsSep, tag, tagsSep) TBR
		}
	}
	return
}

func dbItemFromHget(vals []interface{}, includeStatic bool, logger klog.Logger) (*db_api.BatchItem, error) {

	if (includeStatic && len(vals) != 5) || (!includeStatic && len(vals) != 4) {
		err := fmt.Errorf("unexpected result contents from HMGet: %v", vals)
		logger.Error(err, "dbItemFromHget:")
		return nil, err
	}
	var (
		id, tags, status, spec string
		expiry                 int64
		ok                     bool
	)
	id, ok = vals[0].(string)
	if !ok || len(id) == 0 {
		return nil, nil
	}
	expiry, ok = vals[0].(int64)
	if !ok {
		expiry = 0
	}
	// slo, ok = vals[1].(string) TBD
	// if ok && len(slo) > 0 {
	// 	sloNano, err := strconv.ParseInt(slo, 10, 64)
	// 	if err != nil {
	// 		logger.Error(err, "dbItemFromHget:")
	// 		return nil, err
	// 	}
	// 	sloTime = time.Unix(0, sloNano)
	// }
	tags, ok = vals[1].(string)
	if !ok {
		tags = ""
	}
	status, ok = vals[2].(string)
	if !ok {
		status = ""
	}
	if includeStatic {
		spec, ok = vals[3].(string)
		if !ok {
			spec = ""
		}
	}
	nTags, err := unpackTags(tags)
	if err != nil {
		logger.Error(err, "dbItemFromHget:")
		return nil, err
	}
	job := &db_api.BatchItem{
		ID:     id,
		Expiry: expiry,
		Spec:   []byte(spec),
		Status: []byte(status),
		Tags:   nTags,
	}
	return job, nil
}

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
	if err = item.IsValid(); err != nil {
		logger.Error(err, "PQEnqueue: validation failed")
		return
	}
	zitem := goredis.Z{
		Score:  float64(item.SLO.UnixMicro()),
		Member: data,
	}
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	res := c.redisClient.ZAddNX(cctx, priorityQueueKeyName, zitem)
	ccancel()
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

	if err = item.IsValid(); err != nil {
		logger.Error(err, "PQDelete: validation failed")
		return
	}
	score := strconv.FormatInt(item.SLO.UnixMicro(), 10)
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	res := c.redisClient.ZRemRangeByScore(cctx, priorityQueueKeyName, score, score)
	ccancel()
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

func (c *BatchDSClientRedis) PQDequeue(ctx context.Context, timeout time.Duration, maxObjs int) (
	jobPriorities []*db_api.BatchJobPriority, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)

	// Get items from the queue.
	cctx, ccancel := context.WithTimeout(ctx, timeout+2*time.Second)
	_, vals, err := c.redisClient.BZMPop(
		cctx, timeout, goredis.Min.String(), int64(maxObjs), priorityQueueKeyName).Result()
	ccancel()
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
			logger.Info("PQDequeue: no jobs")
			c.idleLogLast = time.Now()
		}
		return nil, nil
	}

	jobPriorities = make([]*db_api.BatchJobPriority, 0, len(vals))
	for _, val := range vals {
		item := &db_api.BatchJobPriority{}
		err = json.Unmarshal([]byte(val.Member.(string)), item)
		if err != nil {
			logger.Error(err, "PQDelete: Unmarshal failed")
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

func (c *BatchDSClientRedis) ECConsumerGetChannel(ctx context.Context, ID string) (
	batchEventsChan *db_api.BatchEventsChan, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx).WithValues("ID", ID)

	// Create the events listener for the job.
	lctx, lcancel := context.WithCancel(context.Background()) // Use a background context as this should be independent of the context of this call.
	eventChan := make(chan db_api.BatchEvent, eventChanSize)
	stopChan := make(chan any, 1)
	closeFn := func() {
		logger.Info("Listener: close start")
		lcancel() // Signal for listener termination.
		select {
		case <-stopChan: // Wait for listener termination, with a timeout.
		case <-time.After(routineStopTimeout):
		}
		logger.Info("Listener: close end")
	}
	batchEventsChan = &db_api.BatchEventsChan{
		ID:      ID,
		Events:  eventChan,
		CloseFn: closeFn,
	}
	go func() {
		eventsKeyId := getKeyForEvent(ID)
		logger.Info("Listener: start", "eventsKeyId", eventsKeyId)
		for {
			select {
			case <-lctx.Done():
				logger.Info("Listener: received termination signal")
				close(eventChan)
				stopChan <- struct{}{}
				return
			default:
				logger.V(logging.DEBUG).Info("Listener: Start BLMPop")
				lcctx, lccancel := context.WithTimeout(lctx, c.timeout+2*time.Second)
				_, events, err := c.redisClient.BLMPop(lcctx, c.timeout, "left", int64(eventReadCount), eventsKeyId).Result()
				lccancel()
				logger.V(logging.DEBUG).Info("Listener: Finished BLMPop")
				if err != nil {
					if unrecognizedBlockingError(err) {
						logger.Error(err, "Listener: BLMPop failed")
						cerr := c.redisClientChecker.Check(ctx)
						if cerr != nil {
							logger.Error(err, "Listener: ClientCheck failed")
						}
					}
					continue
				}
				for _, event := range events {
					eventi, err := strconv.Atoi(event)
					if err != nil {
						logger.Error(err, "Listener: strconv failed")
						continue
					}
					select {
					case eventChan <- db_api.BatchEvent{
						ID:   ID,
						Type: db_api.BatchEventType(eventi),
					}:
						logger.Info("Listener: dispatched event", "type", event)
					case <-time.After(eventChanTimeout):
						logger.Error(fmt.Errorf("couldn't send event"), "Listener:", "type", event)
					}
				}
			}
		}
	}()

	logger.Info("ECConsumerGetChannel: succeeded")
	return
}

func getKeyForEvent(key string) string {
	return eventKeysPrefix + key
}

func (c *BatchDSClientRedis) ECProducerSendEvents(ctx context.Context, events []db_api.BatchEvent) (
	sentIDs []string, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if len(events) == 0 {
		err = fmt.Errorf("empty events")
		logger.Error(err, "ECProducerSendEvents:")
		return
	}
	for _, event := range events {
		if err = event.IsValid(); err != nil {
			logger.Error(err, "ECProducerSendEvents: invalid event")
			return
		}
	}

	resMap := make(map[string]*goredis.IntCmd)
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	_, err = c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
		for _, event := range events {
			eventTypeStr := strconv.Itoa(int(event.Type))
			key := getKeyForEvent(event.ID)
			res := pipe.RPush(cctx, key, eventTypeStr)
			resMap[event.ID] = res
			pipe.Expire(cctx, key, time.Duration(int64(event.TTL)*int64(time.Second)))
		}
		return nil
	})
	ccancel()
	if err != nil {
		logger.Error(err, "ECProducerSendEvents: Pipelined failed")
		return
	}
	sentIDs = make([]string, 0, len(resMap))
	for id, res := range resMap {
		if res != nil {
			if res.Err() == nil && res.Val() > 0 {
				sentIDs = append(sentIDs, id)
			} else if res.Err() != nil && err == nil {
				err = res.Err()
			}
		}
	}

	logger.Info("ECProducerSendEvents: succeeded", "nIDs", len(sentIDs), "sentIDs", sentIDs)
	return
}

func getKeyForStatus(key string) string {
	return statusKeysPrefix + key
}

func (c *BatchDSClientRedis) STSet(ctx context.Context, ID string, TTL int, data []byte) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if len(ID) == 0 {
		err = fmt.Errorf("empty ID")
		logger.Error(err, "STSet:")
		return
	}
	logger = logger.WithValues("ID", ID)
	if len(data) == 0 {
		err = fmt.Errorf("empty data")
		logger.Error(err, "STSet:")
		return
	}

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	res := c.redisClient.SetEx(cctx, getKeyForStatus(ID), data, time.Duration(int64(TTL)*int64(time.Second)))
	ccancel()
	if res == nil {
		err = fmt.Errorf("nil redis command result")
		logger.Error(err, "STSet:")
		return
	}
	if err = res.Err(); err != nil {
		logger.Error(err, "STSet: redis command error")
		return
	}

	logger.Info("STSet: succeeded")
	return
}

func (c *BatchDSClientRedis) STGet(ctx context.Context, ID string) (data []byte, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if len(ID) == 0 {
		err = fmt.Errorf("empty ID")
		logger.Error(err, "STGet:")
		return
	}
	logger = logger.WithValues("ID", ID)

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	res := c.redisClient.Get(cctx, getKeyForStatus(ID))
	ccancel()
	if res == nil {
		err = fmt.Errorf("nil redis command result")
		logger.Error(err, "STGet:")
		return
	}
	if err = res.Err(); err != nil {
		logger.Error(err, "STGet: redis command error")
		return
	}
	data = []byte(res.Val())

	logger.Info("STGet: succeeded", "len(data)", len(data))
	return
}

func (c *BatchDSClientRedis) STDelete(ctx context.Context, ID string) (nDeleted int, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if len(ID) == 0 {
		err = fmt.Errorf("empty ID")
		logger.Error(err, "STDelete:")
		return
	}
	logger = logger.WithValues("ID", ID)

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	res := c.redisClient.Del(cctx, getKeyForStatus(ID))
	ccancel()
	if res == nil {
		err = fmt.Errorf("nil redis command result")
		logger.Error(err, "STDelete:")
		return
	}
	if err = res.Err(); err != nil {
		logger.Error(err, "STDelete: redis command error")
		return
	}
	nDeleted = int(res.Val())

	logger.Info("STDelete: succeded", "nDeleted", nDeleted)
	return
}
