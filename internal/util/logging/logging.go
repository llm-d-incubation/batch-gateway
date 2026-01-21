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

// The file provides logging utilities and constants for the application.
package logging

import (
	"net/http"

	"k8s.io/klog/v2"
)

const (
	ERROR   = 1
	WARNING = 2
	INFO    = 3
	DEBUG   = 4
	TRACE   = 5
)

type Logger klog.Logger

func GetRequestLogger(r *http.Request) klog.Logger {
	return klog.FromContext(r.Context())
}
