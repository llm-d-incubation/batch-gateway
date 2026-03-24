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

package ctxkeys

import (
	"context"
	"testing"
)

func TestTenantID_Default(t *testing.T) {
	if got := TenantID(context.Background()); got != DefaultTenantID {
		t.Fatalf("TenantID(empty ctx) = %q, want %q", got, DefaultTenantID)
	}
}

func TestTenantID_Set(t *testing.T) {
	ctx := WithTenantID(context.Background(), "tenant-abc")
	if got := TenantID(ctx); got != "tenant-abc" {
		t.Fatalf("TenantID = %q, want %q", got, "tenant-abc")
	}
}

func TestTenantID_EmptyStringFallsBackToDefault(t *testing.T) {
	ctx := WithTenantID(context.Background(), "")
	if got := TenantID(ctx); got != "default" {
		t.Fatalf("TenantID(empty string) = %q, want %q", got, "default")
	}
}

func TestComponent_Default(t *testing.T) {
	if got := Component(context.Background()); got != DefaultComponent {
		t.Fatalf("Component(empty ctx) = %q, want %q", got, DefaultComponent)
	}
}

func TestComponent_Set(t *testing.T) {
	ctx := WithComponent(context.Background(), "processor")
	if got := Component(ctx); got != "processor" {
		t.Fatalf("Component = %q, want %q", got, "processor")
	}
}

func TestComponent_EmptyStringFallsBackToDefault(t *testing.T) {
	ctx := WithComponent(context.Background(), "")
	if got := Component(ctx); got != DefaultComponent {
		t.Fatalf("Component(empty string) = %q, want %q", got, DefaultComponent)
	}
}

func TestRoundTrip_BothKeys(t *testing.T) {
	ctx := WithTenantID(context.Background(), "t1")
	ctx = WithComponent(ctx, "apiserver")

	if got := TenantID(ctx); got != "t1" {
		t.Fatalf("TenantID = %q, want %q", got, "t1")
	}
	if got := Component(ctx); got != "apiserver" {
		t.Fatalf("Component = %q, want %q", got, "apiserver")
	}
}
