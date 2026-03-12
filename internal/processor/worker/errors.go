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

package worker

import "errors"

// Sentinel errors returned by executeJob to signal non-fatal terminal conditions.
// These are checked by runJob / handleJobError to route to the appropriate handler.
// Exported for test access; not referenced outside the processor package.
var (
	ErrCancelled = errors.New("batch job cancelled") // user-initiated cancel
	ErrExpired   = errors.New("batch SLO expired")   // SLO deadline reached during execution
)
