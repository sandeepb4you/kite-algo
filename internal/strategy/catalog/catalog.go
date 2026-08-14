// Package catalog links every built-in strategy into the binary.
//
// Strategies register themselves from an init function, which only runs if the
// package is imported. Listing them here in one place means "which strategies
// does this build ship?" has a single greppable answer, and adding one is a
// one-line change that cannot be forgotten halfway.
//
// Import it for side effects:
//
//	import _ "kite-algo/internal/strategy/catalog"
package catalog

import (
	_ "kite-algo/internal/strategy/examples/shortstraddle"
)
