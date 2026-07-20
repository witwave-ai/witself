// Package homebrew embeds the shared formula template used by release tooling.
package homebrew

import _ "embed"

// FormulaTemplate is the single source template used to render every Witself
// Homebrew formula. Keeping it embedded makes the release tool independent of
// its current working directory while retaining a reviewable Ruby template.
//
//go:embed formula.rb.tmpl
var FormulaTemplate string
