// Package witself holds the repo-root embeds shared across binaries.
//
// PlansJSON embeds the CANONICAL plan catalog — web/plans/plans.json — the
// same file the witself-plans Cloudflare Worker serves publicly. Embedding it
// (rather than copying it) keeps one source of truth: the pricing page, the
// control plane, and the tests all read the identical document, and an invalid
// catalog edit fails the build gates instead of shipping.
package witself

import _ "embed"

// PlansJSON is the raw witself.plans.v0 catalog document. Parse it with
// internal/plans.
//
//go:embed web/plans/plans.json
var PlansJSON []byte
