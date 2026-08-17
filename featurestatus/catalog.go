// Package featurestatus loads and renders the reviewed Witself feature-status
// catalog. The catalog records product progress; it deliberately does not
// replace live fleet, control-plane, or provider health.
package featurestatus

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// SchemaVersion identifies the one feature-status catalog shape understood by
// this package.
const SchemaVersion = "witself.feature-status.v1"

// Implementation, rollout, and gate constants form the closed status
// vocabulary accepted by the catalog validator.
const (
	ImplementationPlanned     = "planned"
	ImplementationSpecified   = "specified"
	ImplementationBuilding    = "building"
	ImplementationImplemented = "implemented"
	ImplementationRetired     = "retired"

	RolloutNotStarted    = "not_started"
	RolloutDark          = "dark"
	RolloutLimited       = "limited"
	RolloutGeneral       = "general"
	RolloutNotApplicable = "not_applicable"
	RolloutRetired       = "retired"

	GatePass          = "pass"
	GateConditional   = "conditional"
	GateFail          = "fail"
	GateNotApplicable = "not_applicable"
)

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	releasePattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
)

//go:embed catalog.json
var canonicalJSON []byte

// Catalog is the reviewed repository declaration for major product features.
type Catalog struct {
	Schema   string    `json:"schema"`
	Features []Feature `json:"features"`
}

// Feature keeps implementation and managed rollout separate. An implemented
// capability may correctly remain dark or limited while its operational gates
// are unfinished.
type Feature struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Area            string     `json:"area"`
	Summary         string     `json:"summary"`
	Implementation  string     `json:"implementation"`
	ManagedRollout  string     `json:"managed_rollout"`
	EvidenceRelease string     `json:"evidence_release,omitempty"`
	EvidenceScope   string     `json:"evidence_scope,omitempty"`
	PlanFeatureKeys []string   `json:"plan_feature_keys,omitempty"`
	PlanLimitKeys   []string   `json:"plan_limit_keys,omitempty"`
	PlanPolicyKeys  []string   `json:"plan_policy_keys,omitempty"`
	Docs            []string   `json:"docs"`
	Gates           Gates      `json:"gates"`
	OpenGates       []OpenGate `json:"open_gates,omitempty"`
}

// Gates are deliberately non-averaged. A feature is operationally accepted
// only when every applicable gate passes.
type Gates struct {
	Behavior          Gate `json:"behavior"`
	EntitlementPolicy Gate `json:"entitlement_policy"`
	BoundsAbuse       Gate `json:"bounds_abuse"`
	Observability     Gate `json:"observability"`
	Recovery          Gate `json:"recovery"`
	RolloutCanaries   Gate `json:"rollout_canaries"`
	DocsSupport       Gate `json:"docs_support"`
}

// Gate contains a compact conclusion and repository evidence. Live health is
// intentionally not copied into this catalog.
type Gate struct {
	State    string   `json:"state"`
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence,omitempty"`
}

// OpenGate is one concrete condition preventing operational acceptance.
type OpenGate struct {
	ID      string   `json:"id"`
	GateIDs []string `json:"gate_ids"`
	Summary string   `json:"summary"`
	Ref     string   `json:"ref"`
}

// NamedGate gives renderers a stable order and human label.
type NamedGate struct {
	ID    string
	Name  string
	Value Gate
}

// OrderedGates returns all mandatory gates in their canonical display order.
func (f Feature) OrderedGates() []NamedGate {
	return []NamedGate{
		{ID: "behavior", Name: "Behavior", Value: f.Gates.Behavior},
		{ID: "entitlement_policy", Name: "Entitlement / policy", Value: f.Gates.EntitlementPolicy},
		{ID: "bounds_abuse", Name: "Bounds / abuse", Value: f.Gates.BoundsAbuse},
		{ID: "observability", Name: "Observability", Value: f.Gates.Observability},
		{ID: "recovery", Name: "Recovery", Value: f.Gates.Recovery},
		{ID: "rollout_canaries", Name: "Rollout / canaries", Value: f.Gates.RolloutCanaries},
		{ID: "docs_support", Name: "Docs / support", Value: f.Gates.DocsSupport},
	}
}

// GateTally summarizes a feature without collapsing conditional or failed
// gates into a misleading completion percentage.
type GateTally struct {
	Pass          int
	Conditional   int
	Fail          int
	NotApplicable int
}

// GateTally returns the feature's readiness-gate counts without averaging
// them into a percentage.
func (f Feature) GateTally() GateTally {
	var out GateTally
	for _, gate := range f.OrderedGates() {
		switch gate.Value.State {
		case GatePass:
			out.Pass++
		case GateConditional:
			out.Conditional++
		case GateFail:
			out.Fail++
		case GateNotApplicable:
			out.NotApplicable++
		}
	}
	return out
}

// Readiness derives the feature's headline state from implementation, rollout,
// and the seven mandatory gates.
func (f Feature) Readiness() string {
	tally := f.GateTally()
	switch {
	case f.Implementation == ImplementationRetired:
		return "retired"
	case tally.Fail > 0:
		return "blocked"
	case f.Implementation != ImplementationImplemented ||
		f.ManagedRollout == RolloutNotStarted || f.ManagedRollout == RolloutDark:
		return "not ready"
	case tally.Conditional > 0:
		return "conditional"
	default:
		return "accepted"
	}
}

// JSON returns a copy of the canonical catalog bytes.
func JSON() []byte { return append([]byte(nil), canonicalJSON...) }

// Load decodes and validates the canonical catalog.
func Load() (*Catalog, error) { return Parse(canonicalJSON) }

// Parse strictly decodes one catalog. Unknown fields and trailing JSON fail.
func Parse(raw []byte) (*Catalog, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var catalog Catalog
	if err := decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode feature status catalog: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode feature status catalog: trailing JSON value")
		}
		return nil, fmt.Errorf("decode feature status catalog trailer: %w", err)
	}
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	return &catalog, nil
}

// Validate checks the stable, reviewable catalog contract. Filesystem
// existence is checked separately by ValidateReferences.
func (c Catalog) Validate() error {
	if c.Schema != SchemaVersion {
		return fmt.Errorf("feature status schema = %q; want %q", c.Schema, SchemaVersion)
	}
	if len(c.Features) == 0 || len(c.Features) > 64 {
		return fmt.Errorf("feature count = %d; want 1..64", len(c.Features))
	}
	seen := make(map[string]bool, len(c.Features))
	previous := ""
	for i, feature := range c.Features {
		if err := feature.validate(); err != nil {
			return fmt.Errorf("feature[%d] %q: %w", i, feature.ID, err)
		}
		if seen[feature.ID] {
			return fmt.Errorf("duplicate feature id %q", feature.ID)
		}
		seen[feature.ID] = true
		if previous != "" && feature.ID < previous {
			return fmt.Errorf("features are not sorted: %q appears after %q", feature.ID, previous)
		}
		previous = feature.ID
	}
	return nil
}

func (f Feature) validate() error {
	if !idPattern.MatchString(f.ID) {
		return fmt.Errorf("invalid id %q", f.ID)
	}
	if err := validateText("name", f.Name, 100); err != nil {
		return err
	}
	if err := validateText("area", f.Area, 80); err != nil {
		return err
	}
	if err := validateText("summary", f.Summary, 320); err != nil {
		return err
	}
	if !slices.Contains([]string{
		ImplementationPlanned, ImplementationSpecified, ImplementationBuilding,
		ImplementationImplemented, ImplementationRetired,
	}, f.Implementation) {
		return fmt.Errorf("invalid implementation %q", f.Implementation)
	}
	if !slices.Contains([]string{
		RolloutNotStarted, RolloutDark, RolloutLimited, RolloutGeneral,
		RolloutNotApplicable, RolloutRetired,
	}, f.ManagedRollout) {
		return fmt.Errorf("invalid managed rollout %q", f.ManagedRollout)
	}
	if f.Implementation == ImplementationRetired && f.ManagedRollout != RolloutRetired {
		return errors.New("retired implementation requires retired rollout")
	}
	if f.Implementation != ImplementationRetired && f.ManagedRollout == RolloutRetired {
		return errors.New("retired rollout requires retired implementation")
	}
	if f.EvidenceRelease != "" && !releasePattern.MatchString(f.EvidenceRelease) {
		return fmt.Errorf("invalid evidence release %q", f.EvidenceRelease)
	}
	if (f.EvidenceRelease == "") != (f.EvidenceScope == "") {
		return errors.New("evidence_release and evidence_scope must be set together")
	}
	if f.EvidenceScope != "" {
		if err := validateText("evidence_scope", f.EvidenceScope, 240); err != nil {
			return err
		}
	}
	if len(f.Docs) == 0 || len(f.Docs) > 12 {
		return fmt.Errorf("docs count = %d; want 1..12", len(f.Docs))
	}
	if err := validateSortedReferences("docs", f.Docs); err != nil {
		return err
	}
	if err := validateSortedIDs("plan_feature_keys", f.PlanFeatureKeys); err != nil {
		return err
	}
	if err := validateSortedIDs("plan_limit_keys", f.PlanLimitKeys); err != nil {
		return err
	}
	if err := validateSortedIDs("plan_policy_keys", f.PlanPolicyKeys); err != nil {
		return err
	}
	conditionalOrFailed := false
	gateStates := make(map[string]string, len(f.OrderedGates()))
	requiredOpenGateCoverage := make(map[string]bool)
	for _, named := range f.OrderedGates() {
		if err := named.Value.validate(); err != nil {
			return fmt.Errorf("gate %s: %w", named.ID, err)
		}
		gateStates[named.ID] = named.Value.State
		conditionalOrFailed = conditionalOrFailed ||
			named.Value.State == GateConditional || named.Value.State == GateFail
		if named.Value.State == GateConditional || named.Value.State == GateFail {
			requiredOpenGateCoverage[named.ID] = false
		}
	}
	if len(f.OpenGates) > 12 {
		return fmt.Errorf("open gate count = %d; want no more than 12", len(f.OpenGates))
	}
	if conditionalOrFailed && len(f.OpenGates) == 0 {
		return errors.New("conditional or failed gates require at least one open_gates entry")
	}
	if !conditionalOrFailed && len(f.OpenGates) != 0 {
		return errors.New("accepted feature cannot retain open_gates entries")
	}
	previous := ""
	seen := make(map[string]bool, len(f.OpenGates))
	for i, gate := range f.OpenGates {
		if !idPattern.MatchString(gate.ID) {
			return fmt.Errorf("open_gates[%d] invalid id %q", i, gate.ID)
		}
		if seen[gate.ID] {
			return fmt.Errorf("duplicate open gate id %q", gate.ID)
		}
		seen[gate.ID] = true
		if previous != "" && gate.ID < previous {
			return fmt.Errorf("open gates are not sorted: %q appears after %q", gate.ID, previous)
		}
		previous = gate.ID
		if err := validateText("open gate summary", gate.Summary, 300); err != nil {
			return err
		}
		if len(gate.GateIDs) == 0 || len(gate.GateIDs) > 7 {
			return fmt.Errorf("open gate %q gate_ids count = %d; want 1..7", gate.ID, len(gate.GateIDs))
		}
		if err := validateSortedIDs("open gate "+gate.ID+" gate_ids", gate.GateIDs); err != nil {
			return err
		}
		for _, gateID := range gate.GateIDs {
			state, ok := gateStates[gateID]
			if !ok {
				return fmt.Errorf("open gate %q references unknown gate %q", gate.ID, gateID)
			}
			if state != GateConditional && state != GateFail {
				return fmt.Errorf("open gate %q references %s gate %q", gate.ID, state, gateID)
			}
			requiredOpenGateCoverage[gateID] = true
		}
		if err := validateReference(gate.Ref); err != nil {
			return fmt.Errorf("open gate %q ref: %w", gate.ID, err)
		}
	}
	for gateID, covered := range requiredOpenGateCoverage {
		if !covered {
			return fmt.Errorf("conditional or failed gate %q has no linked open_gates entry", gateID)
		}
	}
	if !conditionalOrFailed && f.Implementation != ImplementationRetired {
		if f.Implementation != ImplementationImplemented {
			return errors.New("all applicable gates pass but implementation is not implemented")
		}
		if f.ManagedRollout == RolloutNotStarted || f.ManagedRollout == RolloutDark || f.ManagedRollout == RolloutRetired {
			return errors.New("all applicable gates pass but managed rollout is not active")
		}
		if f.EvidenceRelease == "" {
			return errors.New("accepted managed rollout requires evidence_release and evidence_scope")
		}
	}
	if f.Implementation == ImplementationRetired {
		for _, named := range f.OrderedGates() {
			if named.Value.State != GateNotApplicable {
				return fmt.Errorf("retired feature gate %q is %q; want not_applicable", named.ID, named.Value.State)
			}
		}
	}
	return nil
}

func (g Gate) validate() error {
	if !slices.Contains([]string{GatePass, GateConditional, GateFail, GateNotApplicable}, g.State) {
		return fmt.Errorf("invalid state %q", g.State)
	}
	if err := validateText("summary", g.Summary, 320); err != nil {
		return err
	}
	if len(g.Evidence) > 8 {
		return fmt.Errorf("evidence count = %d; want no more than 8", len(g.Evidence))
	}
	if g.State != GateNotApplicable && len(g.Evidence) == 0 {
		return errors.New("applicable gate requires evidence")
	}
	return validateSortedReferences("evidence", g.Evidence)
}

func validateText(name, value string, maximum int) error {
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s has leading or trailing whitespace", name)
	}
	if value == "" {
		return fmt.Errorf("%s is empty", name)
	}
	if len(value) > maximum {
		return fmt.Errorf("%s is %d bytes; maximum is %d", name, len(value), maximum)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s contains a newline", name)
	}
	return nil
}

func validateSortedIDs(name string, values []string) error {
	previous := ""
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !idPattern.MatchString(strings.ReplaceAll(value, "_", "-")) {
			return fmt.Errorf("%s contains invalid key %q", name, value)
		}
		if seen[value] {
			return fmt.Errorf("%s contains duplicate %q", name, value)
		}
		seen[value] = true
		if previous != "" && value < previous {
			return fmt.Errorf("%s is not sorted: %q appears after %q", name, value, previous)
		}
		previous = value
	}
	return nil
}

func validateSortedReferences(name string, values []string) error {
	previous := ""
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if err := validateReference(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if seen[value] {
			return fmt.Errorf("%s contains duplicate %q", name, value)
		}
		seen[value] = true
		if previous != "" && value < previous {
			return fmt.Errorf("%s is not sorted: %q appears after %q", name, value, previous)
		}
		previous = value
	}
	return nil
}

func validateReference(value string) error {
	if strings.TrimSpace(value) != value || value == "" || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("invalid reference %q", value)
	}
	if strings.HasPrefix(value, "https://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("invalid HTTPS reference %q", value)
		}
		return nil
	}
	path := strings.SplitN(value, "#", 2)[0]
	if filepath.IsAbs(path) || path == "." || strings.HasPrefix(path, "../") || strings.Contains(path, "\\") {
		return fmt.Errorf("invalid repository-relative reference %q", value)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("invalid repository-relative reference %q", value)
		}
	}
	if pathpkg.Clean(path) != path {
		return fmt.Errorf("unclean repository-relative reference %q", value)
	}
	return nil
}

// ValidateReferences verifies every repository-relative reference against an
// explicit repository root. URLs remain reviewable external evidence.
func (c Catalog) ValidateReferences(repoRoot string) error {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve repository root symlinks: %w", err)
	}
	for _, feature := range c.Features {
		refs := append([]string(nil), feature.Docs...)
		for _, gate := range feature.OrderedGates() {
			refs = append(refs, gate.Value.Evidence...)
		}
		for _, gate := range feature.OpenGates {
			refs = append(refs, gate.Ref)
		}
		for _, ref := range refs {
			if strings.HasPrefix(ref, "https://") {
				continue
			}
			path := strings.SplitN(ref, "#", 2)[0]
			target, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(path)))
			if err != nil {
				return fmt.Errorf("feature %q reference %q: resolve target: %w", feature.ID, ref, err)
			}
			relative, err := filepath.Rel(root, target)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("feature %q reference %q escapes repository root", feature.ID, ref)
			}
			info, err := os.Lstat(target)
			if err != nil {
				return fmt.Errorf("feature %q reference %q: %w", feature.ID, ref, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("feature %q reference %q is a symbolic link", feature.ID, ref)
			}
			resolvedTarget, err := filepath.EvalSymlinks(target)
			if err != nil {
				return fmt.Errorf("feature %q reference %q: resolve symlinks: %w", feature.ID, ref, err)
			}
			resolvedRelative, err := filepath.Rel(resolvedRoot, resolvedTarget)
			if err != nil || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("feature %q reference %q resolves outside repository root", feature.ID, ref)
			}
			if info.IsDir() {
				return fmt.Errorf("feature %q reference %q is a directory", feature.ID, ref)
			}
		}
	}
	return nil
}
