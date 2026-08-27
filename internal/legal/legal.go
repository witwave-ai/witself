// Package legal pins the compiled-in version labels of the public legal
// documents the CLI can record consent to at signup.
//
// These constants MUST be bumped whenever the content under docs/legal/
// changes materially, and at general-availability cutover they MUST match the
// version labels shown on the published Terms of Service and Privacy Policy
// pages — the recorded consent is only meaningful if it names the exact text
// the user saw. The pages are currently DRAFTS, so the versions say so
// honestly rather than implying final legal review happened.
package legal

// TermsVersion is the version label of the Terms of Service draft that
// `witself account create --accept-terms` records consent to.
const TermsVersion = "draft-2026-08-22"

// PrivacyVersion is the version label of the Privacy Policy draft that
// `witself account create --accept-terms` records consent to.
const PrivacyVersion = "draft-2026-08-22"
