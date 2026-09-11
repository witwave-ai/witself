// Package gitopsvalues generates per-cell .gitops/cells/<cell>/values.yaml
// overlays from the checked-in cell catalog. It emits exact text from
// cloud-family templates and per-cell overlays so comments and key order
// survive; it does not dump YAML through yq.
package gitopsvalues
