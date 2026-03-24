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

// Package ctxkeys defines shared context keys used across all components
// (apiserver, processor, GC) to propagate request-scoped metadata such as
// tenant identity and component name.
//
// TODO: Consolidate apiserver/common.RequestIDKey and common.TenantIDKey
// into this package so all context keys live in one place. Once consolidated,
// remove the duplicate context.WithValue(ctx, common.TenantIDKey, tenantID) in
// request_middleware.go and have common.GetTenantIDFromContext delegate to
// ctxkeys.TenantID.
package ctxkeys

import "context"

// Key is a typed string to avoid collisions with other context values.
type Key string

const (
	TenantIDKey  Key = "tenantID"
	ComponentKey Key = "component"

	DefaultTenantID  = "default"
	DefaultComponent = "unknown"
)

// TenantID extracts the tenant ID from ctx, returning DefaultTenantID if absent.
func TenantID(ctx context.Context) string {
	if v, ok := ctx.Value(TenantIDKey).(string); ok && v != "" {
		return v
	}
	return DefaultTenantID
}

// Component extracts the component name from ctx, returning DefaultComponent if absent.
func Component(ctx context.Context) string {
	if v, ok := ctx.Value(ComponentKey).(string); ok && v != "" {
		return v
	}
	return DefaultComponent
}

// WithTenantID returns a child context carrying the given tenant ID.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, TenantIDKey, tenantID)
}

// WithComponent returns a child context carrying the given component name.
func WithComponent(ctx context.Context, component string) context.Context {
	return context.WithValue(ctx, ComponentKey, component)
}
