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

// This file provides a redis event channel client implementation.

package redis

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	db_api "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	goredis "github.com/redis/go-redis/v9"
)

func (c *ExchangeDBClientRedis) ECConsumerGetChannel(ctx context.Context, ID string) (
	batchEventsChan *db_api.BatchEventsChan, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx).WithValues("ID", ID)

	// Create the events listener for the job.
	lctx, lcancel := context.WithCancel(context.Background()) // Use a background context as this should be independent of the context of this call.
	eventChan := make(chan db_api.BatchEvent, eventChanSize)
	stopChan := make(chan any, 1)
	var once sync.Once
	closeFn := func() {
		once.Do(func() {
			logger.Info("Listener: close start")
			lcancel() // Signal for listener termination.
			select {
			case <-stopChan: // Wait for listener termination, with a timeout.
			case <-time.After(routineStopTimeout):
			}
			logger.Info("Listener: close end")
		})
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
				_, events, err := c.redisClient.BLMPop(lctx, eventReadTimeout, "left", int64(eventReadCount), eventsKeyId).Result()
				logger.V(logging.DEBUG).Info("Listener: Finished BLMPop")
				if err != nil {
					if unrecognizedBlockingError(err) {
						logger.Error(err, "Listener: BLMPop failed")
						cerr := c.redisClientChecker.Check(lctx)
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
					afterTimer := time.After(eventChanTimeout)
					select {
					case eventChan <- db_api.BatchEvent{
						ID:   ID,
						Type: db_api.BatchEventType(eventi),
					}:
						logger.Info("Listener: dispatched event", "type", event)
					case <-afterTimer:
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

func (c *ExchangeDBClientRedis) ECProducerSendEvents(ctx context.Context, events []db_api.BatchEvent) (
	sentIDs []string, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := logr.FromContextOrDiscard(ctx)
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
	defer ccancel()
	_, err = c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
		for _, event := range events {
			eventTypeStr := strconv.Itoa(int(event.Type))
			key := getKeyForEvent(event.ID)
			res := pipe.RPush(cctx, key, eventTypeStr)
			resMap[event.ID] = res
			pipe.Expire(cctx, key, time.Duration(event.TTL)*time.Second)
		}
		return nil
	})
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
