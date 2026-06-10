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

package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	db_api "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
)

func (c *ExchangeDBClientRedis) InFlightSet(ctx context.Context, jobID, processorID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if jobID == "" {
		return fmt.Errorf("jobID is empty")
	}
	if processorID == "" {
		return fmt.Errorf("processorID is empty")
	}

	entry := db_api.InFlightEntry{
		ProcessorID: processorID,
		LastSeen:    time.Now().Unix(),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("failed to marshal in-flight entry: %w", err)
	}

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	return c.redisClient.HSet(cctx, inFlightKeyName, jobID, data).Err()
}

func (c *ExchangeDBClientRedis) InFlightDelete(ctx context.Context, jobID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if jobID == "" {
		return fmt.Errorf("jobID is empty")
	}

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()
	return c.redisClient.HDel(cctx, inFlightKeyName, jobID).Err()
}

func (c *ExchangeDBClientRedis) InFlightGetAll(ctx context.Context) (map[string]*db_api.InFlightEntry, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	result := make(map[string]*db_api.InFlightEntry)
	var cursor uint64
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	defer ccancel()

	for {
		entries, nextCursor, err := c.redisClient.HScan(cctx, inFlightKeyName, cursor, "*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("failed to scan in-flight hash: %w", err)
		}
		// HScan returns [field, value, field, value, ...]
		for i := 0; i < len(entries); i += 2 {
			jobID := entries[i]
			var entry db_api.InFlightEntry
			if err := json.Unmarshal([]byte(entries[i+1]), &entry); err != nil {
				return nil, fmt.Errorf("failed to unmarshal in-flight entry for job %s: %w", jobID, err)
			}
			result[jobID] = &entry
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	return result, nil
}
