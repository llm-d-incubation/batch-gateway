//go:build ignore

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// NoMagicVLevel flags .V() calls with literal integer arguments > 0.
// V(0) is allowed (logr convention). Use named constants from
// internal/util/logging (INFO=1, DEBUG=3, TRACE=5) instead.
func NoMagicVLevel(m dsl.Matcher) {
	m.Match(`$_.V($lvl)`).
		Where(m["lvl"].Text.Matches(`^[1-9]`)).
		Report("Use named constants from internal/util/logging (INFO=1, DEBUG=3, TRACE=5) instead of magic V-level numbers.")
}
