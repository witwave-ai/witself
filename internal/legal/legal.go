// Package legal pins the compiled-in version labels of the public legal
// documents the CLI can record consent to at signup.
//
// Each affected constant MUST be bumped whenever its document under docs/legal/
// changes materially, and MUST match the version label shown on the
// published pages at https://self.witwave.ai/legal — the recorded consent is
// only meaningful if it names the exact text the user saw. Preserve prior text
// under docs/legal/versions/<version>/ and register a new content fingerprint in
// testdata/published-versions.json rather than changing a published fingerprint.
// `witself legal` reads the pages from the terminal, and /legal/versions.json is
// the canonical manifest.
package legal

// TermsVersion is the version label of the published Terms of Service that
// `witself account create --accept-terms` records consent to.
const TermsVersion = "2026-08-31"

// PrivacyVersion is the version label of the published Privacy Policy that
// `witself account create --accept-terms` records consent to.
const PrivacyVersion = "2026-09-04"

// BaseURL is where the published legal pages are served. Each document is
// BaseURL/<slug>; ?format=md returns the raw markdown, and
// BaseURL/versions.json is the version manifest.
const BaseURL = "https://self.witwave.ai/legal"
