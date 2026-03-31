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

// NoRedundantV0Info flags V(0).Info() — V(0) is the default level for
// Info, so the V(0) call is redundant. Use .Info() directly.
func NoRedundantV0Info(m dsl.Matcher) {
	m.Match(`$_.V(0).Info($*_)`).
		Report("V(0).Info() is redundant; V(0) is the default Info level. Use .Info() directly.")
}

// NoVLevelOnError flags .V($n).Error() — Error logs should never be
// gated behind a verbosity level.
func NoVLevelOnError(m dsl.Matcher) {
	m.Match(`$_.V($lvl).Error($*_)`).
		Report("Do not use V() with Error(); errors should always be logged regardless of verbosity.")
}

// NoNilError flags .Error(nil, ...) — if there is no error, use Info
// instead. Error() should always receive a real error.
func NoNilError(m dsl.Matcher) {
	m.Match(`$_.Error(nil, $*_)`).
		Report("Do not pass nil to Error(); if there is no error, use Info() instead.")
}
