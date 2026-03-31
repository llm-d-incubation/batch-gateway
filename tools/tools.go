//go:build tools

// Package tools pins tool dependencies that are not imported by regular Go source
// but are required by the build/lint pipeline (e.g. gorules loaded by golangci-lint).
package tools

import _ "github.com/quasilyte/go-ruleguard/dsl"
