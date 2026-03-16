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

// Package com provides factory functions for creating all external clients used by
// the batch gateway apiserver and processor. Centralising client construction here
// ensures both processes use identical setup logic.
package clientset

import (
	"context"
	"errors"
	"fmt"

	dbapi "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/database/postgresql"
	dbRedis "github.com/llm-d-incubation/batch-gateway/internal/database/redis"
	fsapi "github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
	fsclient "github.com/llm-d-incubation/batch-gateway/internal/files_store/fs"
	s3client "github.com/llm-d-incubation/batch-gateway/internal/files_store/s3"
	ucom "github.com/llm-d-incubation/batch-gateway/internal/util/com"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	"github.com/llm-d-incubation/batch-gateway/pkg/clients/inference"
	"k8s.io/klog/v2"
)

// Clientset holds all clients.
type Clientset struct {
	File      fsapi.BatchFilesClient
	BatchDB   dbapi.BatchDBClient
	FileDB    dbapi.FileDBClient
	Queue     dbapi.BatchPriorityQueueClient
	Event     dbapi.BatchEventChannelClient
	Status    dbapi.BatchStatusClient
	Inference *inference.GatewayResolver
}

// NewClientset creates all clients.
func NewClientset(
	ctx context.Context,
	dbType string,
	postgreSQLCfg *postgresql.PostgreSQLConfig,
	redisCfg *uredis.RedisClientConfig,
	fileClientType string,
	fsCfg *fsclient.Config,
	s3Cfg *s3client.Config,
	modelGatewaysConfigs map[string]inference.GatewayClientConfig,
) (*Clientset, error) {

	logger := klog.FromContext(ctx)

	// check required parameters
	if redisCfg == nil {
		return nil, fmt.Errorf("redisCfg cannot be nil")
	}

	cs := &Clientset{}

	// TODO: The exchange interfaces (priority queue, events, status) currently always use Redis.
	// Consider adding a separate type parameter for these if we need alternative backends.
	// See: https://github.com/llm-d-incubation/batch-gateway/pull/102#discussion_r2906181334

	// build redis client
	if redisCfg.Url == "" {
		redisURL, err := ucom.ReadSecretFile(ucom.SecretKeyRedisURL)
		if err != nil {
			return nil, err
		}
		redisCfg.Url = redisURL
	}
	redisClient, err := dbRedis.NewExchangeDBClientRedis(ctx, nil, redisCfg, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to create redis DS client: %w", err)
	}
	logger.Info("Redis client created")
	cs.Queue = redisClient
	cs.Event = redisClient
	cs.Status = redisClient

	// build file store client
	switch fileClientType {
	case "fs":
		if fsCfg == nil {
			return nil, fmt.Errorf("fsCfg cannot be nil when file_client.type is \"fs\"")
		}
		if err := fsCfg.Validate(); err != nil {
			return nil, fmt.Errorf("invalid fs config: %w", err)
		}
		c, err := fsclient.New(fsCfg.BasePath)
		if err != nil {
			return nil, fmt.Errorf("failed to create fs file client: %w", err)
		}
		logger.Info("Filesystem-based file client created", "base_path", fsCfg.BasePath)
		cs.File = c

	case "s3":
		if s3Cfg == nil {
			return nil, fmt.Errorf("s3Cfg cannot be nil when file_client.type is \"s3\"")
		}
		if s3Cfg.SecretAccessKey == "" {
			s3SecretAccessKey, err := ucom.ReadSecretFile(ucom.SecretKeyS3SecretAccessKey)
			if err != nil {
				return nil, fmt.Errorf("failed to read S3 secret access key: %w", err)
			}
			s3Cfg.SecretAccessKey = s3SecretAccessKey
		}
		if err := s3Cfg.Validate(); err != nil {
			return nil, fmt.Errorf("invalid s3 config: %w", err)
		}
		c, err := s3client.New(ctx, *s3Cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create s3 file client: %w", err)
		}
		logger.Info("S3 file client created", "region", s3Cfg.Region, "endpoint", s3Cfg.Endpoint)
		cs.File = c

	default:
		return nil, fmt.Errorf("unsupported file_client.type: %s (supported values: fs, s3)", fileClientType)
	}

	// build database client
	switch dbType {
	case "redis":
		batchDB, err := dbRedis.NewBatchDBClientRedis(ctx, nil, redisCfg, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to create redis batch-db client: %w", err)
		}
		fileDB, err := dbRedis.NewFileDBClientRedis(ctx, nil, redisCfg, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to create redis file-db client: %w", err)
		}
		cs.BatchDB = batchDB
		cs.FileDB = fileDB
		logger.Info("Redis-based database client created")
	case "postgresql":
		if postgreSQLCfg == nil {
			return nil, fmt.Errorf("postgreSQLCfg cannot be nil when database.type is \"postgresql\"")
		}
		if postgreSQLCfg.Url == "" {
			postgreSQLURL, err := ucom.ReadSecretFile(ucom.SecretKeyPostgreSQLURL)
			if err != nil {
				return nil, err
			}
			postgreSQLCfg.Url = postgreSQLURL
		}
		batchDB, err := postgresql.NewPostgresBatchDBClient(ctx, postgreSQLCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create postgresql batch-db client: %w", err)
		}
		fileDB, err := postgresql.NewPostgresFileDBClient(ctx, postgreSQLCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create postgresql file-db client: %w", err)
		}
		cs.BatchDB = batchDB
		cs.FileDB = fileDB
		logger.Info("PostgreSQL-based database client created")
	default:
		return nil, fmt.Errorf("unsupported database.type: %s (supported values: redis, postgresql)", dbType)
	}

	// build inference client(s)
	if modelGatewaysConfigs != nil {
		resolver, err := inference.NewGatewayResolver(modelGatewaysConfigs)
		if err != nil {
			return nil, fmt.Errorf("failed to create inference client(s): %w", err)
		}
		logger.Info("Inference client(s) created")
		cs.Inference = resolver
	}

	return cs, nil
}

func (cs *Clientset) Close() error {
	var errs []error
	if cs.Queue != nil {
		if err := cs.Queue.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if cs.Event != nil {
		if err := cs.Event.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if cs.Status != nil {
		if err := cs.Status.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if cs.BatchDB != nil {
		if err := cs.BatchDB.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if cs.FileDB != nil {
		if err := cs.FileDB.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if cs.File != nil {
		if err := cs.File.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
