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
	"sync"
)

// worker id is integer that starts with 1 to the max number of worker
type WorkerPool struct {
	workerIds chan int
	wg        sync.WaitGroup
}

func NewWorkerPool(maxWorkers int) *WorkerPool {
	ids := make(chan int, maxWorkers)
	for i := 1; i <= maxWorkers; i++ {
		ids <- i // fill worker ids first
	}
	return &WorkerPool{
		workerIds: ids,
	}
}

// return worker id and bool showing if the acquisition was succesful
// id 0 means the worker was not acquired
func (wp *WorkerPool) TryAcquire() (int, bool) {
	select {
	case id := <-wp.workerIds:
		wp.wg.Add(1)
		return id, true
	default:
		return 0, false
	}
}

func (wp *WorkerPool) Release(id int) {
	wp.workerIds <- id
	wp.wg.Done()
}

func (wp *WorkerPool) WaitAll() {
	wp.wg.Wait()
}
