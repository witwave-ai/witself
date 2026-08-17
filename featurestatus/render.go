package featurestatus

import (
	"bytes"
	"fmt"
	"path"
	"strings"
)

// RenderMarkdown renders the canonical human-readable projection. The JSON
// catalog remains authoritative and this output is checked byte-for-byte in CI.
func RenderMarkdown(c Catalog) []byte {
	var out bytes.Buffer
	out.WriteString("<!-- Code generated from featurestatus/catalog.json; DO NOT EDIT. -->\n\n")
	out.WriteString("# Feature Status\n\n")
	out.WriteString("This scorecard is the canonical reviewed declaration for current commercial capabilities, substantial implemented or actively building product surfaces, and specified security foundations whose contracts constrain delivered behavior. Other deferred candidates stay in the post-v0 roadmap. It separates implementation from managed rollout and applies the same seven non-averaged readiness gates to every tracked feature. Live fleet, control-plane, Cloudflare, provider, and billing health remain authoritative in their operational systems; this document does not turn a point-in-time observation into permanent truth.\n\n")
	out.WriteString("The source is [`featurestatus/catalog.json`](../featurestatus/catalog.json). Run `make feature-status` after changing it. Ordinary `go test ./...` also rejects an invalid catalog, missing reference, uncovered plan feature, or stale generated document.\n\n")
	out.WriteString("## Reading the scorecard\n\n")
	out.WriteString("- **Implementation**: `planned`, `specified`, `building`, `implemented`, or `retired`.\n")
	out.WriteString("- **Managed rollout**: `not started`, `dark`, `limited`, `general`, `not applicable`, or `retired`. This is a reviewed declaration, not a live-health claim.\n")
	out.WriteString("- **Readiness**: `accepted` only for an implemented active or not-applicable rollout whose applicable gates all pass and whose release evidence is scoped; `conditional` when an implemented non-dark rollout has work remaining; `not ready` for planned/building or dark work; `blocked` when a gate fails; and `retired` for retired work. We do not average gates into a completion percentage.\n")
	out.WriteString("- **Seven gates**: behavior, entitlement/policy, bounds/abuse, observability, recovery, rollout/canaries, and docs/support. `N/A` requires a written rationale.\n\n")
	out.WriteString("A feature being implemented does not mean it is generally available. A plan entitlement also does not prove that its feature is ready or enabled for a particular account.\n\n")
	out.WriteString("## Summary\n\n")
	out.WriteString("| Feature | Area | Implementation | Managed rollout | Readiness | Gates | Open gates |\n")
	out.WriteString("|---|---|---|---|---|---:|---:|\n")
	for _, feature := range c.Features {
		tally := feature.GateTally()
		applicable := 7 - tally.NotApplicable
		fmt.Fprintf(&out, "| [%s](#%s) | %s | `%s` | `%s` | **%s** | %d/%d pass | %d |\n",
			escapeTable(feature.Name), feature.ID, escapeTable(feature.Area),
			feature.Implementation, strings.ReplaceAll(feature.ManagedRollout, "_", " "),
			feature.Readiness(), tally.Pass, applicable, len(feature.OpenGates))
	}
	out.WriteString("\n## Feature details\n\n")
	for _, feature := range c.Features {
		fmt.Fprintf(&out, "<a id=\"%s\"></a>\n\n", feature.ID)
		fmt.Fprintf(&out, "### %s\n\n", feature.Name)
		fmt.Fprintf(&out, "%s\n\n", feature.Summary)
		fmt.Fprintf(&out, "- Implementation: `%s`\n", feature.Implementation)
		fmt.Fprintf(&out, "- Managed rollout: `%s`\n", strings.ReplaceAll(feature.ManagedRollout, "_", " "))
		fmt.Fprintf(&out, "- Readiness: **%s**\n", feature.Readiness())
		if feature.EvidenceRelease != "" {
			fmt.Fprintf(&out, "- Retained release/cohort evidence: `%s` — %s\n", feature.EvidenceRelease, feature.EvidenceScope)
		}
		if len(feature.PlanFeatureKeys) != 0 {
			fmt.Fprintf(&out, "- Plan feature keys: `%s`\n", strings.Join(feature.PlanFeatureKeys, "`, `"))
		}
		if len(feature.PlanLimitKeys) != 0 {
			fmt.Fprintf(&out, "- Plan limit keys: `%s`\n", strings.Join(feature.PlanLimitKeys, "`, `"))
		}
		if len(feature.PlanPolicyKeys) != 0 {
			fmt.Fprintf(&out, "- Plan policy keys: `%s`\n", strings.Join(feature.PlanPolicyKeys, "`, `"))
		}
		fmt.Fprintf(&out, "- Detailed docs: %s\n\n", renderReferences(feature.Docs))
		out.WriteString("| Gate | State | Current evidence and conclusion |\n")
		out.WriteString("|---|---|---|\n")
		for _, gate := range feature.OrderedGates() {
			summary := escapeTable(gate.Value.Summary)
			if len(gate.Value.Evidence) != 0 {
				summary += " " + renderReferences(gate.Value.Evidence)
			}
			fmt.Fprintf(&out, "| %s | **%s** | %s |\n", gate.Name, gateLabel(gate.Value.State), summary)
		}
		out.WriteString("\n")
		if len(feature.OpenGates) == 0 {
			out.WriteString("Open gates: none.\n\n")
			continue
		}
		out.WriteString("Open gates:\n\n")
		for _, gate := range feature.OpenGates {
			fmt.Fprintf(&out, "- `%s` (%s): %s ([tracking/evidence](%s))\n", gate.ID, renderGateIDs(gate.GateIDs), gate.Summary, linkTarget(gate.Ref))
		}
		out.WriteString("\n")
	}
	return append(bytes.TrimRight(out.Bytes(), "\n"), '\n')
}

func renderGateIDs(ids []string) string {
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		labels = append(labels, strings.ReplaceAll(id, "_", " / "))
	}
	return strings.Join(labels, ", ")
}

func gateLabel(state string) string {
	switch state {
	case GatePass:
		return "PASS"
	case GateConditional:
		return "CONDITIONAL"
	case GateFail:
		return "FAIL"
	case GateNotApplicable:
		return "N/A"
	default:
		return state
	}
}

func renderReferences(refs []string) string {
	links := make([]string, 0, len(refs))
	for _, ref := range refs {
		label := ref
		if strings.HasPrefix(ref, "https://") {
			parts := strings.Split(strings.TrimSuffix(ref, "/"), "/")
			if len(parts) >= 2 {
				label = strings.Join(parts[len(parts)-2:], " #")
			}
		} else {
			base := strings.SplitN(ref, "#", 2)[0]
			label = path.Base(base)
		}
		links = append(links, fmt.Sprintf("[%s](%s)", label, linkTarget(ref)))
	}
	return strings.Join(links, ", ")
}

func linkTarget(ref string) string {
	if strings.HasPrefix(ref, "https://") {
		return ref
	}
	return "../" + ref
}

func escapeTable(value string) string {
	return strings.ReplaceAll(value, "|", "\\|")
}
