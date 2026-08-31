// Package legal pins the compiled-in version labels of the public legal
// documents the CLI can record consent to at signup.
//
// These constants MUST be bumped whenever the content under docs/legal/
// changes materially, and they MUST match the version labels shown on the
// published pages at https://self.witwave.ai/legal — the recorded consent is
// only meaningful if it names the exact text the user saw. The pages were
// ratified and published as Version 2026-08-31; `witself legal` reads them
// from the terminal, and /legal/versions.json is the canonical manifest.
package legal

// TermsVersion is the version label of the published Terms of Service that
// `witself account create --accept-terms` records consent to.
const TermsVersion = "2026-08-31"

// PrivacyVersion is the version label of the published Privacy Policy that
// `witself account create --accept-terms` records consent to.
const PrivacyVersion = "2026-08-31"

// BaseURL is where the published legal pages are served. Each document is
// BaseURL/<slug>; ?format=md returns the raw markdown, and
// BaseURL/versions.json is the version manifest.
const BaseURL = "https://self.witwave.ai/legal"
