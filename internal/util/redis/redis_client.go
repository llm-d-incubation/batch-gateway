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

// This file provides redis client utilities.

package redis

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	ucom "github.com/llm-d-incubation/batch-gateway/internal/util/com"
	utls "github.com/llm-d-incubation/batch-gateway/internal/util/tls"
	"github.com/redis/go-redis/extra/redisotel/v9"
	gredis "github.com/redis/go-redis/v9"
	"k8s.io/klog/v2"
)

const (
	REDIS_PING_WAIT_SEC = 10
)

type RedisClientConfig struct {
	Url             string             `yaml:"-"` // Resolved from K8s Secret at runtime
	Certificates    *utls.Certificates `yaml:"-"` // Set programmatically for mTLS
	EnableTracing   bool               `yaml:"-"` // Set from OTel config
	ServiceName     string             `yaml:"-"` // Set per component (e.g. "batch-apiserver")
	DB              int                `yaml:"db"`
	EnableTLS       bool               `yaml:"enable_tls"`
	Insecure        bool               `yaml:"insecure"`
	Timeout         time.Duration      `yaml:"timeout"`            // Timeout for socket operations: dial, read, write.
	MaxRetries      int                `yaml:"max_retries"`        // Maximum number of retries before giving up. Default is 3 retries; -1 (not 0) disables retries.
	MinRetryBackoff time.Duration      `yaml:"min_retry_backoff"`  // Minimum backoff between each retry. Default is 8 milliseconds; -1 disables backoff.
	MaxRetryBackoff time.Duration      `yaml:"max_retry_backoff"`  // Maximum backoff between each retry. Default is 512 milliseconds; -1 disables backoff.
	PoolTimeout     time.Duration      `yaml:"pool_timeout"`       // Amount of time client waits for connection if all connections are busy before returning an error. Default is ReadTimeout + 1 second.
	ConnMaxIdleTime time.Duration      `yaml:"conn_max_idle_time"` // The maximum amount of time a connection may be idle. If <= 0, connections are not closed due to a connection's idle time. Default is 30 minutes. -1 disables idle timeout check.
	ConnMaxLifetime time.Duration      `yaml:"conn_max_lifetime"`  // The maximum amount of time a connection may be reused. If <= 0, connections are not closed due to a connection's age. Default is to not close idle connections.
}

func NewRedisClient(ctx context.Context, cnf *RedisClientConfig) (*gredis.Client, error) {
	var (
		redisOps  *gredis.Options
		tlsConfig *tls.Config
		err       error
	)
	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if cnf == nil {
		err = fmt.Errorf("redis config was not provided")
		logger.Error(err, "NewRedisClient")
		return nil, err
	}
	if cnf.Url == "" {
		err = fmt.Errorf("redis config has empty url")
		logger.Error(err, "NewRedisClient")
		return nil, err
	}
	redisOps, err = gredis.ParseURL(cnf.Url)
	if err != nil {
		logger.Error(err, "NewRedisClient")
		return nil, err
	}
	if redisOps.ClientName == "" {
		hostname, _ := os.Hostname()
		if cnf.ServiceName != "" {
			redisOps.ClientName = fmt.Sprintf("%s-%s-%d-%s", cnf.ServiceName, hostname, os.Getpid(), ucom.RandString(6))
		} else {
			redisOps.ClientName = fmt.Sprintf("%s-%d-%s", hostname, os.Getpid(), ucom.RandString(6))
		}
	}
	if cnf.DB >= 0 {
		redisOps.DB = cnf.DB
	}
	if cnf.Timeout != 0 {
		redisOps.DialTimeout = cnf.Timeout
		redisOps.ReadTimeout = cnf.Timeout
		redisOps.WriteTimeout = cnf.Timeout
	}
	redisOps.ContextTimeoutEnabled = true
	if cnf.MaxRetries != 0 {
		redisOps.MaxRetries = cnf.MaxRetries
	}
	if cnf.MinRetryBackoff != 0 {
		redisOps.MinRetryBackoff = cnf.MinRetryBackoff
	}
	if cnf.MaxRetryBackoff != 0 {
		redisOps.MaxRetryBackoff = cnf.MaxRetryBackoff
	}
	if cnf.PoolTimeout != 0 {
		redisOps.PoolTimeout = cnf.PoolTimeout
	}
	if cnf.ConnMaxIdleTime != 0 {
		redisOps.ConnMaxIdleTime = cnf.ConnMaxIdleTime
	}
	if cnf.ConnMaxLifetime != 0 {
		redisOps.ConnMaxLifetime = cnf.ConnMaxLifetime
	}
	if cnf.EnableTLS {
		certFile, keyFile, caCertFile := "", "", ""
		if cnf.Certificates != nil && !cnf.Certificates.IsEmpty() {
			certCf := cnf.Certificates
			certFile = utls.JoinCertPath(certCf.Dir, certCf.CertFile)
			keyFile = utls.JoinCertPath(certCf.Dir, certCf.KeyFile)
			caCertFile = utls.JoinCertPath(certCf.Dir, certCf.CaCertFile)
		}
		tlsConfig, err = utls.GetTlsConfig(
			utls.LOAD_TYPE_CLIENT,
			cnf.Insecure,
			certFile,
			keyFile,
			caCertFile,
		)
		if err != nil {
			logger.Error(err, "NewRedisClient")
			return nil, err
		}
	}
	if tlsConfig != nil {
		redisOps.TLSConfig = tlsConfig
	}
	rds := gredis.NewClient(redisOps)
	if rds == nil {
		err = fmt.Errorf("redis.NewClient returned nil client [addr: %s]", redisOps.Addr)
		logger.Error(err, "NewRedisClient")
		return nil, err
	}
	if cnf.EnableTracing {
		if err := redisotel.InstrumentTracing(rds); err != nil {
			logger.Error(err, "NewRedisClient: failed to instrument Redis tracing")
			_ = rds.Close()
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), REDIS_PING_WAIT_SEC*time.Second)
	defer cancel()
	_, err = rds.Ping(ctx).Result()
	if err != nil {
		logger.Error(err, "NewRedisClient")
		_ = rds.Close()
		return nil, err
	}
	logger.Info("NewRedisClient", "clientName", redisOps.ClientName)
	return rds, nil
}

func CheckClient(ctx context.Context, rds *gredis.Client, cmdTimeout time.Duration, keyPrefix, serviceName string) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	cctx, ccancel := context.WithTimeout(ctx, cmdTimeout)
	err = rds.Set(cctx, getPingKeyName(keyPrefix, serviceName), "ping", 10*time.Second).Err()
	ccancel()
	if err == nil {
		var info string
		cctx, ccancel = context.WithTimeout(ctx, cmdTimeout)
		info, err = rds.Info(cctx, "replication").Result()
		ccancel()
		if err == nil {
			if !strings.Contains(info, "role:slave") {
				// In this case the client is determined as good.
				logger.Info("CheckClient: success")
				return nil
			} else {
				err = fmt.Errorf("slave confirmed")
			}
		} else {
			err = fmt.Errorf("info failed: %s", err.Error())
		}
	} else {
		err = fmt.Errorf("write failed: %s", err.Error())
	}
	logger.Error(err, "CheckClient: failed")
	return
}

func getPingKeyName(prefix, serviceName string) string {
	return fmt.Sprintf("%sping:%s:%s", prefix, serviceName, time.Now().Format("20060102150405"))
}

type RedisClientChecker struct {
	rds         *gredis.Client
	lock        *sync.Mutex
	keyPrefix   string
	serviceName string
	cmdTimeout  time.Duration
}

func NewRedisClientChecker(rds *gredis.Client, keyPrefix, serviceName string, cmdTimeout time.Duration) *RedisClientChecker {
	return &RedisClientChecker{
		rds:         rds,
		lock:        &sync.Mutex{},
		keyPrefix:   keyPrefix,
		serviceName: serviceName,
		cmdTimeout:  cmdTimeout,
	}
}

func (r *RedisClientChecker) Check(ctx context.Context) (err error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	return CheckClient(ctx, r.rds, r.cmdTimeout, r.keyPrefix, r.serviceName)
}
