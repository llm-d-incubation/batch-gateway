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

// file explains;;

package worker

import (
    "context"
    "time"

    "k8s.io/klog/v2"

    "github.com/llm-d-incubation/batch-gateway/internal/processor/config"
    "github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
)

type WorkerPool struct {
    sem chan struct{}
    wg sync.WaitGroup
}


func (wp *WorkerPool) TryAcquire() bool {
    select {
    case wp.sem <- struct{}{}:
        wp.wg.Add(1)
        return true
    default:
        return false
    }
}

func (wp *WorkerPool) Release() {
    <-wp.sem
    wp.wg.Done()
}

func (wp *WorkerPool) WaitAll() {
    wp.wg.Wait()
}
