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

// Package clientset provides factory functions for creating all external clients
// used by the batch gateway apiserver and processor. Centralising client
// construction here ensures both processes use identical setup logic.
package clientset

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	dbapi "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/database/postgresql"
	dbRedis "github.com/llm-d-incubation/batch-gateway/internal/database/redis"
	fsapi "github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
	fsclient "github.com/llm-d-incubation/batch-gateway/internal/files_store/fs"
	"github.com/llm-d-incubation/batch-gateway/internal/files_store/retryclient"
	s3client "github.com/llm-d-incubation/batch-gateway/internal/files_store/s3"
	fstracing "github.com/llm-d-incubation/batch-gateway/internal/files_store/tracing"
	sharedcfg "github.com/llm-d-incubation/batch-gateway/internal/shared/config"
	ucom "github.com/llm-d-incubation/batch-gateway/internal/util/com"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	"github.com/llm-d-incubation/batch-gateway/pkg/clients/inference"
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

// NewFSFileClient creates a filesystem-based file storage client.
func NewFSFileClient(ctx context.Context, cfg *fsclient.Config) (fsapi.BatchFilesClient, error) {
	if cfg == nil {
		return nil, fmt.Errorf("fs config cannot be nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid fs config: %w", err)
	}
	c, err := fsclient.New(cfg.BasePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create fs file client: %w", err)
	}
	logr.FromContextOrDiscard(ctx).Info("Filesystem-based file client created", "base_path", cfg.BasePath)
	return c, nil
}

// NewS3FileClient creates an S3-based file storage client.
// It reads the secret access key from the mounted secrets when not set in the config.
func NewS3FileClient(ctx context.Context, cfg *s3client.Config) (fsapi.BatchFilesClient, error) {
	if cfg == nil {
		return nil, fmt.Errorf("s3 config cannot be nil")
	}
	if cfg.SecretAccessKey == "" {
		s3SecretAccessKey, err := ucom.ReadSecretFile(ucom.SecretKeyS3SecretAccessKey)
		if err != nil {
			return nil, fmt.Errorf("failed to read S3 secret access key: %w", err)
		}
		cfg.SecretAccessKey = s3SecretAccessKey
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid s3 config: %w", err)
	}
	c, err := s3client.New(ctx, *cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create s3 file client: %w", err)
	}
	logr.FromContextOrDiscard(ctx).Info("S3 file client created", "region", cfg.Region, "endpoint", cfg.Endpoint)
	return c, nil
}

// NewRedisDBClients creates Redis-backed batch and file database clients.
// It reads the Redis URL from the mounted secrets when not set in the config.
func NewRedisDBClients(ctx context.Context, cfg *uredis.RedisClientConfig) (dbapi.BatchDBClient, dbapi.FileDBClient, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("redis config cannot be nil")
	}
	if cfg.Url == "" {
		redisURL, err := ucom.ReadSecretFile(ucom.SecretKeyRedisURL)
		if err != nil {
			return nil, nil, err
		}
		cfg.Url = redisURL
	}
	batchDB, err := dbRedis.NewBatchDBClientRedis(ctx, nil, cfg, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create redis batch-db client: %w", err)
	}
	fileDB, err := dbRedis.NewFileDBClientRedis(ctx, nil, cfg, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create redis file-db client: %w", err)
	}
	logr.FromContextOrDiscard(ctx).Info("Redis-based database client created")
	return batchDB, fileDB, nil
}

// NewPostgreSQLDBClients creates PostgreSQL-backed batch and file database clients.
// It reads the URL from the mounted secrets when not set in the config.
func NewPostgreSQLDBClients(ctx context.Context, cfg *postgresql.PostgreSQLConfig) (dbapi.BatchDBClient, dbapi.FileDBClient, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("postgresql config cannot be nil")
	}
	if cfg.Url == "" {
		postgreSQLURL, err := ucom.ReadSecretFile(ucom.SecretKeyPostgreSQLURL)
		if err != nil {
			return nil, nil, err
		}
		cfg.Url = postgreSQLURL
	}
	batchDB, err := postgresql.NewPostgresBatchDBClient(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create postgresql batch-db client: %w", err)
	}
	fileDB, err := postgresql.NewPostgresFileDBClient(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create postgresql file-db client: %w", err)
	}
	logr.FromContextOrDiscard(ctx).Info("PostgreSQL-based database client created")
	return batchDB, fileDB, nil
}

// Option configures which clients NewClientset creates.
type Option func(*clientsetConfig)

type clientsetConfig struct {
	dbCfg             *sharedcfg.DBClientConfig
	fileCfg           *sharedcfg.FileClientConfig
	exchangeRedisCfg  *uredis.RedisClientConfig
	inferenceGlobal   *inference.GatewayClientConfig
	inferencePerModel map[string]inference.GatewayClientConfig
}

// WithDB enables creation of batch and file database clients.
func WithDB(cfg sharedcfg.DBClientConfig) Option {
	cfg = cfg.DeepCopy()
	return func(c *clientsetConfig) { c.dbCfg = &cfg }
}

// WithFile enables creation of the file storage client.
func WithFile(cfg sharedcfg.FileClientConfig) Option {
	return func(c *clientsetConfig) { c.fileCfg = &cfg }
}

// WithExchange enables creation of the Redis exchange client (Queue, Event, Status).
func WithExchange(cfg uredis.RedisClientConfig) Option {
	cfg = cfg.DeepCopy()
	return func(c *clientsetConfig) { c.exchangeRedisCfg = &cfg }
}

// WithGlobalInference enables creation of a global inference client.
func WithGlobalInference(cfg inference.GatewayClientConfig) Option {
	return func(c *clientsetConfig) { c.inferenceGlobal = &cfg }
}

// WithPerModelInference enables creation of per-model inference clients.
func WithPerModelInference(cfgs map[string]inference.GatewayClientConfig) Option {
	copied := make(map[string]inference.GatewayClientConfig, len(cfgs))
	for k, v := range cfgs {
		copied[k] = v
	}
	return func(c *clientsetConfig) { c.inferencePerModel = copied }
}

// NewClientset creates the clients specified by the given options.
func NewClientset(ctx context.Context, component ucom.Component, opts ...Option) (*Clientset, error) {
	logger := logr.FromContextOrDiscard(ctx)

	cfg := &clientsetConfig{}
	for _, o := range opts {
		o(cfg)
	}

	cs := &Clientset{}

	// build redis exchange client
	if cfg.exchangeRedisCfg != nil {
		// TODO: The exchange interfaces (priority queue, events, status) currently always use Redis.
		// Consider adding a separate type parameter for these if we need alternative backends.
		// See: https://github.com/llm-d-incubation/batch-gateway/pull/102#discussion_r2906181334
		if cfg.exchangeRedisCfg.Url == "" {
			redisURL, err := ucom.ReadSecretFile(ucom.SecretKeyRedisURL)
			if err != nil {
				return nil, err
			}
			cfg.exchangeRedisCfg.Url = redisURL
		}
		redisClient, err := dbRedis.NewExchangeDBClientRedis(ctx, nil, cfg.exchangeRedisCfg, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to create redis exchange client: %w", err)
		}
		logger.Info("Redis exchange client created")
		cs.Queue = redisClient
		cs.Event = redisClient
		cs.Status = redisClient
	}

	// build file store client
	if cfg.fileCfg != nil {
		switch cfg.fileCfg.Type {
		case sharedcfg.FileTypeFS:
			c, err := NewFSFileClient(ctx, &cfg.fileCfg.FSConfig)
			if err != nil {
				return nil, err
			}
			cs.File = fstracing.Wrap(c, sharedcfg.FileTypeFS)
		case sharedcfg.FileTypeS3:
			c, err := NewS3FileClient(ctx, &cfg.fileCfg.S3Config)
			if err != nil {
				return nil, err
			}
			cs.File = fstracing.Wrap(c, sharedcfg.FileTypeS3)
		default:
			return nil, fmt.Errorf("unsupported file_client.type: %s (supported values: fs, s3)", cfg.fileCfg.Type)
		}
		if cfg.fileCfg.Retry.MaxRetries > 0 {
			cs.File = retryclient.New(cs.File, cfg.fileCfg.Retry, component)
			logger.Info("File client wrapped with retry", "maxRetries", cfg.fileCfg.Retry.MaxRetries)
		}
	}

	// build database client
	if cfg.dbCfg != nil {
		switch cfg.dbCfg.Type {
		case sharedcfg.DBTypeRedis, sharedcfg.DBTypeValkey:
			redisCfg := &cfg.dbCfg.RedisCfg
			batchDB, fileDB, err := NewRedisDBClients(ctx, redisCfg)
			if err != nil {
				return nil, err
			}
			cs.BatchDB = batchDB
			cs.FileDB = fileDB
		case sharedcfg.DBTypePostgreSQL:
			batchDB, fileDB, err := NewPostgreSQLDBClients(ctx, &cfg.dbCfg.PostgreSQLCfg)
			if err != nil {
				return nil, err
			}
			cs.BatchDB = batchDB
			cs.FileDB = fileDB
		default:
			return nil, fmt.Errorf("unsupported database.type: %s (supported values: redis, valkey, postgresql)", cfg.dbCfg.Type)
		}
	}

	// build inference client(s)
	switch {
	case cfg.inferenceGlobal != nil:
		resolver, err := inference.NewGlobalResolver(*cfg.inferenceGlobal, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to create global inference client: %w", err)
		}
		logger.Info("Global inference client created")
		cs.Inference = resolver
	case len(cfg.inferencePerModel) > 0:
		resolver, err := inference.NewPerModelResolver(cfg.inferencePerModel, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to create per-model inference clients: %w", err)
		}
		logger.Info("Per-model inference clients created", "count", len(cfg.inferencePerModel))
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
