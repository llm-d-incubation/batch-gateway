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

// Package ptr provides generic helpers for working with pointer values.
package ptr

// To returns a pointer to the given value.
func To[T any](v T) *T { return &v }

// DerefOr returns the pointed-to value, or fallback if p is nil.
func DerefOr[T any](p *T, fallback T) T {
	if p != nil {
		return *p
	}
	return fallback
}

// Deref returns the pointed-to value, or the zero value of T if p is nil.
func Deref[T any](p *T) T {
	var zero T
	return DerefOr(p, zero)
}
