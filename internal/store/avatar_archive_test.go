package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

const (
	avatarArchiveAccount  = "acc_avatar_archive"
	avatarArchiveRealm    = "realm-avatar-archive"
	avatarArchiveAgent    = "agent_avatar_archive"
	avatarArchiveOperator = "operator_avatar_archive"
	avatarArchiveTime     = "2026-07-17T12:00:00Z"
)

func TestAvatarArchiveValidationAcceptsCanonicalPendingEvolution(t *testing.T) {
	ic := newAvatarArchiveImportContext(t)
	feedAvatarArchiveStyle(t, ic, false)
	feedAvatarArchiveProfile(t, ic, map[string]any{
		"status": "proposed", "subject_form": "human",
		"latest_avatar_version": int64(2), "proposed_avatar_version": int64(2),
		"active_avatar_version": int64(1), "revision": int64(5),
	})
	feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
	feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman))
	feedAvatarArchiveActivation(t, ic, map[string]any{
		"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
		"prior_active_version": nil, "action": "activated",
	})

	if err := ic.validateImportedAvatarGraph(); err != nil {
		t.Fatalf("canonical pending evolution graph = %v", err)
	}
}

func TestAvatarArchiveValidationAcceptsResetAndFreshLineage(t *testing.T) {
	ic := newAvatarArchiveImportContext(t)
	feedAvatarArchiveStyle(t, ic, false)
	feedAvatarArchiveProfile(t, ic, map[string]any{
		"status": "proposed", "lineage_generation": int64(2),
		"subject_form": "animal", "latest_avatar_version": int64(3),
		"proposed_avatar_version": int64(3), "revision": int64(8),
	})
	feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
	feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman))
	feedAvatarArchiveActivation(t, ic, map[string]any{
		"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
		"prior_active_version": nil, "action": "activated",
	})
	feedAvatarArchiveReset(t, ic, map[string]any{
		"retired_active_version": int64(1), "retired_proposed_version": int64(2),
	})
	fresh := avatarArchiveVersionRow(t, 3, 0, avatardomain.SubjectAnimal)
	fresh["lineage_generation"] = int64(2)
	feedAvatarArchiveVersion(t, ic, fresh)

	if err := ic.validateImportedAvatarGraph(); err != nil {
		t.Fatalf("fresh avatar lineage graph = %v", err)
	}
}

func TestAvatarArchiveValidationAcceptsFirstActivationAfterReset(t *testing.T) {
	ic := newAvatarArchiveImportContext(t)
	feedAvatarArchiveStyle(t, ic, false)
	feedAvatarArchiveProfile(t, ic, map[string]any{
		"status": "active", "lineage_generation": int64(2),
		"latest_avatar_version": int64(2), "active_avatar_version": int64(2),
		"revision": int64(7),
	})
	feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
	feedAvatarArchiveActivation(t, ic, map[string]any{
		"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
		"prior_active_version": nil, "action": "activated",
	})
	feedAvatarArchiveReset(t, ic, map[string]any{"retired_active_version": int64(1)})
	fresh := avatarArchiveVersionRow(t, 2, 0, avatardomain.SubjectHuman)
	fresh["lineage_generation"] = int64(2)
	feedAvatarArchiveVersion(t, ic, fresh)
	feedAvatarArchiveActivation(t, ic, map[string]any{
		"id": "avact_bbbbbbbbbbbbbbbb", "sequence": int64(2),
		"lineage_generation": int64(2), "avatar_version": int64(2),
		"prior_active_version": nil, "action": "activated",
	})

	if err := ic.validateImportedAvatarGraph(); err != nil {
		t.Fatalf("first activation after reset = %v", err)
	}
}

func TestAvatarArchiveValidationRejectsForgedResetLineage(t *testing.T) {
	t.Run("profile lineage requires reset ledger", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"lineage_generation": int64(2), "revision": int64(2),
		})
		if err := ic.validateImportedAvatarGraph(); err == nil || !strings.Contains(err.Error(), "reset count") {
			t.Fatalf("error = %v, want missing reset refusal", err)
		}
	})

	t.Run("version parent cannot cross lineage", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"lineage_generation": int64(2), "latest_avatar_version": int64(2),
			"status": "proposed", "proposed_avatar_version": int64(2),
			"revision": int64(4),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		row := avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman)
		row["lineage_generation"] = int64(2)
		err := ic.validateAndRecord("agent_avatar_versions", row)
		if err == nil || !strings.Contains(err.Error(), "crosses avatar lineages") {
			t.Fatalf("error = %v, want cross-lineage parent refusal", err)
		}
	})

	t.Run("reset must retire a pointer", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{"lineage_generation": int64(2)})
		err := ic.validateAndRecord("agent_avatar_resets", avatarArchiveResetRow(map[string]any{}))
		if err == nil || !strings.Contains(err.Error(), "retires neither") {
			t.Fatalf("error = %v, want empty reset refusal", err)
		}
	})

	t.Run("reset cannot predate retired lifecycle activity", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"lineage_generation": int64(2), "latest_avatar_version": int64(1),
			"revision": int64(4),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})
		row := avatarArchiveResetRow(map[string]any{
			"retired_active_version": int64(1),
			"reset_at":               "2026-07-17T11:59:59Z",
		})
		err := ic.validateAndRecord("agent_avatar_resets", row)
		if err == nil || !strings.Contains(err.Error(), "precedes lifecycle activity") {
			t.Fatalf("error = %v, want retired-lineage chronology refusal", err)
		}
	})

	t.Run("new lineage activity cannot predate reset", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"lineage_generation": int64(2), "latest_avatar_version": int64(2),
			"revision": int64(4),
		})
		first := avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman)
		first["proposed_at"] = "2026-07-17T11:00:00Z"
		feedAvatarArchiveVersion(t, ic, first)
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
			"activated_at": "2026-07-17T11:00:01Z",
		})
		fresh := avatarArchiveVersionRow(t, 2, 0, avatardomain.SubjectHuman)
		fresh["lineage_generation"] = int64(2)
		fresh["proposed_at"] = "2026-07-17T11:59:59Z"
		feedAvatarArchiveVersion(t, ic, fresh)
		row := avatarArchiveResetRow(map[string]any{
			"retired_active_version": int64(1), "reset_at": avatarArchiveTime,
		})
		err := ic.validateAndRecord("agent_avatar_resets", row)
		if err == nil || !strings.Contains(err.Error(), "activity precedes its reset boundary") {
			t.Fatalf("error = %v, want new-lineage chronology refusal", err)
		}
	})
}

func TestAvatarArchiveValidationEnforcesAgentAuthoredContinuity(t *testing.T) {
	newContext := func(t *testing.T) *importCtx {
		t.Helper()
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		return ic
	}

	t.Run("same style subject change", func(t *testing.T) {
		ic := newContext(t)
		row := avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectAnimal)
		err := ic.validateAndRecord("agent_avatar_versions", row)
		if err == nil || !strings.Contains(err.Error(), "changes subject_form") {
			t.Fatalf("error = %v, want same-style subject refusal", err)
		}
	})

	t.Run("same style locked layer change", func(t *testing.T) {
		ic := newContext(t)
		row := avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman)
		svg := strings.Replace(row["svg"].(string), `r="220" fill="#DCEAF5"`, `r="210" fill="#DCEAF5"`, 1)
		row["svg"] = svg
		digest := sha256.Sum256([]byte(svg))
		row["svg_sha256"] = hex.EncodeToString(digest[:])
		lockedDigest, digestErr := avatardomain.LockedLayersSHA256(
			[]byte(svg), avatardomain.BuiltInFlatVectorStylePack())
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		row["locked_layers_sha256"] = lockedDigest
		err := ic.validateAndRecord("agent_avatar_versions", row)
		if err == nil || !strings.Contains(err.Error(), "locked-layer continuity") {
			t.Fatalf("error = %v, want locked-layer refusal", err)
		}
	})

	t.Run("same style unlocked identity occlusion", func(t *testing.T) {
		ic := newContext(t)
		row := avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman)
		svg := strings.Replace(row["svg"].(string),
			`<g id="experience" data-layer="experience"></g>`,
			`<g id="experience" data-layer="experience"><circle cx="256" cy="230" r="136" fill="#F7FAFC"></circle></g>`, 1)
		setAvatarArchiveVersionSVG(t, row, svg)
		err := ic.validateAndRecord("agent_avatar_versions", row)
		if err == nil || !strings.Contains(err.Error(), "perceptual continuity") {
			t.Fatalf("error = %v, want perceptual continuity refusal", err)
		}
	})

	t.Run("full payload locked digest must match normalized projection", func(t *testing.T) {
		ic := newContext(t)
		row := avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman)
		row["locked_layers_sha256"] = strings.Repeat("0", 64)
		err := ic.validateAndRecord("agent_avatar_versions", row)
		if err == nil || !strings.Contains(err.Error(), "locked_layers_sha256 mismatch") {
			t.Fatalf("error = %v, want locked digest mismatch", err)
		}
	})

	t.Run("compacted parent retains structural and perceptual continuity boundary", func(t *testing.T) {
		newCompactedContext := func(t *testing.T) *importCtx {
			t.Helper()
			ic := newAvatarArchiveImportContext(t)
			feedAvatarArchiveStyle(t, ic, false)
			feedAvatarArchiveProfile(t, ic, map[string]any{})
			parent := avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman)
			parentFingerprint := avatarArchiveContinuityFingerprint(t, parent["svg"].(string))
			parent["payload_state"] = string(avatardomain.PayloadCompacted)
			parent["svg"], parent["description"], parent["visual_spec"] = nil, nil, nil
			parent["payload_compacted_at"] = "2026-07-17T12:30:00Z"
			parent["payload_compaction_reason"] = "quota"
			parent["continuity_fingerprint"] = parentFingerprint
			feedAvatarArchiveVersion(t, ic, parent)
			return ic
		}

		ic := newCompactedContext(t)
		if err := ic.validateAndRecord("agent_avatar_versions",
			avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman)); err != nil {
			t.Fatalf("matching compacted-parent boundary was rejected: %v", err)
		}

		ic = newCompactedContext(t)
		child := avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman)
		svg := strings.Replace(child["svg"].(string),
			`r="220" fill="#DCEAF5"`, `r="210" fill="#DCEAF5"`, 1)
		child["svg"] = svg
		svgDigest := sha256.Sum256([]byte(svg))
		child["svg_sha256"] = hex.EncodeToString(svgDigest[:])
		lockedDigest, err := avatardomain.LockedLayersSHA256(
			[]byte(svg), avatardomain.BuiltInFlatVectorStylePack())
		if err != nil {
			t.Fatal(err)
		}
		child["locked_layers_sha256"] = lockedDigest
		err = ic.validateAndRecord("agent_avatar_versions", child)
		if err == nil || !strings.Contains(err.Error(), "retained locked-layer continuity") {
			t.Fatalf("error = %v, want compacted boundary refusal", err)
		}

		ic = newCompactedContext(t)
		occluded := avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman)
		svg = strings.Replace(occluded["svg"].(string),
			`<g id="experience" data-layer="experience"></g>`,
			`<g id="experience" data-layer="experience"><circle cx="256" cy="230" r="136" fill="#F7FAFC"></circle></g>`, 1)
		setAvatarArchiveVersionSVG(t, occluded, svg)
		err = ic.validateAndRecord("agent_avatar_versions", occluded)
		if err == nil || !strings.Contains(err.Error(), "compacted-parent perceptual continuity") {
			t.Fatalf("error = %v, want compacted-parent occlusion refusal", err)
		}
	})

	t.Run("operator override", func(t *testing.T) {
		ic := newContext(t)
		row := avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectAnimal)
		row["proposed_by_kind"] = PrincipalOperator
		row["proposed_by_id"] = avatarArchiveOperator
		if err := ic.validateAndRecord("agent_avatar_versions", row); err != nil {
			t.Fatalf("operator override was rejected: %v", err)
		}
	})

	t.Run("operator perceptual override", func(t *testing.T) {
		ic := newContext(t)
		row := avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman)
		svg := strings.Replace(row["svg"].(string),
			`<g id="experience" data-layer="experience"></g>`,
			`<g id="experience" data-layer="experience"><circle cx="256" cy="230" r="136" fill="#F7FAFC"></circle></g>`, 1)
		setAvatarArchiveVersionSVG(t, row, svg)
		row["proposed_by_kind"] = PrincipalOperator
		row["proposed_by_id"] = avatarArchiveOperator
		if err := ic.validateAndRecord("agent_avatar_versions", row); err != nil {
			t.Fatalf("operator perceptual override was rejected: %v", err)
		}
	})

	t.Run("style version migration", func(t *testing.T) {
		ic := newContext(t)
		pack := avatardomain.BuiltInFlatVectorStylePack()
		pack.Version = 2
		style := avatarStyleVersionImportKey{
			realmID: avatarArchiveRealm, stylePackID: pack.ID, version: 2,
		}
		ic.avatarStyleVersions[style] = avatarStyleVersionImportScope{pack: pack, previousVersion: 1}
		row := avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectAnimal)
		row["style_pack_version"] = int64(2)
		if err := ic.validateAndRecord("agent_avatar_versions", row); err != nil {
			t.Fatalf("style-version migration was rejected: %v", err)
		}
	})
}

func TestImportedAvatarContinuityFingerprintValidation(t *testing.T) {
	pack := avatardomain.BuiltInFlatVectorStylePack()
	fingerprint, err := avatardomain.BuildPerceptualContinuityFingerprint(
		[]byte(pack.References[0].SVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	encoded := `\x` + hex.EncodeToString(fingerprint)
	decoded, err := importedAvatarContinuityFingerprint(map[string]any{
		"continuity_fingerprint": encoded,
	})
	if err != nil || !bytes.Equal(decoded, fingerprint) {
		t.Fatalf("valid fingerprint = %d bytes / %v", len(decoded), err)
	}
	badChecksum := append([]byte(nil), fingerprint...)
	badChecksum[len(badChecksum)-1] ^= 0xff
	for name, value := range map[string]any{
		"wrong type":     1,
		"missing hex":    encoded[2:],
		"bad hex":        `\x` + strings.Repeat("00", len(fingerprint)-1) + "0z",
		"wrong length":   `\x` + hex.EncodeToString(fingerprint[:len(fingerprint)-1]),
		"bad checksum":   `\x` + hex.EncodeToString(badChecksum),
		"trailing bytes": encoded + "00",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := importedAvatarContinuityFingerprint(map[string]any{
				"continuity_fingerprint": value,
			}); err == nil {
				t.Fatal("invalid fingerprint was accepted")
			}
		})
	}

	t.Run("style mismatch", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{})
		other := pack
		other.Name += " alternate"
		foreign, err := avatardomain.BuildPerceptualContinuityFingerprint(
			[]byte(other.References[0].SVG), other)
		if err != nil {
			t.Fatal(err)
		}
		row := compactAvatarArchiveVersionRow(t, 1, 0, foreign)
		err = ic.validateAndRecord("agent_avatar_versions", row)
		if err == nil || !strings.Contains(err.Error(), "does not match the avatar style") {
			t.Fatalf("error = %v, want fingerprint style refusal", err)
		}
	})

	t.Run("full row forbidden", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{})
		row := avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman)
		row["continuity_fingerprint"] = encoded
		err := ic.validateAndRecord("agent_avatar_versions", row)
		if err == nil || !strings.Contains(err.Error(), "full payload carries continuity_fingerprint") {
			t.Fatalf("error = %v, want full-row fingerprint refusal", err)
		}
	})
}

func TestAvatarArchiveGraphRequiresPerceptualFingerprintExactlyWhenNeeded(t *testing.T) {
	pack := avatardomain.BuiltInFlatVectorStylePack()
	fingerprint, err := avatardomain.BuildPerceptualContinuityFingerprint(
		[]byte(pack.References[0].SVG), pack)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("missing compacted parent boundary", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "proposed", "subject_form": "human",
			"latest_avatar_version": int64(2), "proposed_avatar_version": int64(2),
			"active_avatar_version": int64(1), "revision": int64(5),
		})
		feedAvatarArchiveVersion(t, ic, compactAvatarArchiveVersionRow(t, 1, 0, fingerprint))
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman))
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})

		key := avatarVersionImportKey{agentID: avatarArchiveAgent, version: 1}
		parent := ic.avatarVersions[key]
		parent.continuityFingerprint = nil
		ic.avatarVersions[key] = parent
		err := ic.validateImportedAvatarGraph()
		if err == nil || !strings.Contains(err.Error(), "lacks its required continuity_fingerprint") {
			t.Fatalf("error = %v, want missing compacted-parent fingerprint refusal", err)
		}
	})

	t.Run("obsolete compacted parent boundary", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "active", "subject_form": "human",
			"latest_avatar_version": int64(1), "active_avatar_version": int64(1),
			"revision": int64(3),
		})
		feedAvatarArchiveVersion(t, ic, compactAvatarArchiveVersionRow(t, 1, 0, fingerprint))
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})

		err := ic.validateImportedAvatarGraph()
		if err == nil || !strings.Contains(err.Error(), "retains an obsolete continuity_fingerprint") {
			t.Fatalf("error = %v, want obsolete compacted-parent fingerprint refusal", err)
		}
	})
}

func TestAvatarArchiveGraphIndexesLargeManyAgentHistoryLinearly(t *testing.T) {
	const agentCount = 5_000
	versions := make(map[avatarVersionImportKey]avatarVersionImportScope, agentCount*2)
	style := avatarStyleVersionImportKey{
		realmID: avatarArchiveRealm, stylePackID: avatardomain.DefaultStylePackID, version: 1,
	}
	for i := 0; i < agentCount; i++ {
		agentID := fmt.Sprintf("agent_archive_scale_%05d", i)
		versions[avatarVersionImportKey{agentID: agentID, version: 1}] = avatarVersionImportScope{
			lineage: 1, style: style, payloadState: avatardomain.PayloadCompacted,
		}
		versions[avatarVersionImportKey{agentID: agentID, version: 2}] = avatarVersionImportScope{
			lineage: 1, style: style, parentVersion: 1,
			payloadState: avatardomain.PayloadFull, proposedByKind: PrincipalAgent,
		}
	}
	required := requiredImportedAvatarContinuityFingerprints(versions)
	grouped := importedAvatarVersionsByAgent(versions)
	if len(required) != agentCount || len(grouped) != agentCount {
		t.Fatalf("large archive indexes = required:%d agents:%d, want %d",
			len(required), len(grouped), agentCount)
	}
	for agentID, entries := range grouped {
		if len(entries) != 2 || entries[0].key.version != 1 || entries[1].key.version != 2 {
			t.Fatalf("agent %q grouped versions = %#v", agentID, entries)
		}
	}
}

func TestAvatarArchiveValidationRejectsLifecycleAfterPayloadCompaction(t *testing.T) {
	newCompactedContext := func(t *testing.T, profile map[string]any) *importCtx {
		t.Helper()
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, profile)
		version := avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman)
		version["payload_state"] = string(avatardomain.PayloadCompacted)
		version["svg"], version["description"], version["visual_spec"] = nil, nil, nil
		version["payload_compacted_at"] = "2026-07-17T12:10:00Z"
		version["payload_compaction_reason"] = "quota"
		feedAvatarArchiveVersion(t, ic, version)
		return ic
	}

	t.Run("activation", func(t *testing.T) {
		ic := newCompactedContext(t, map[string]any{
			"status": "active", "latest_avatar_version": int64(1),
			"active_avatar_version": int64(1), "revision": int64(2),
		})
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
			"activated_at": "2026-07-17T12:20:00Z",
		})
		err := ic.validateImportedAvatarGraph()
		if err == nil || !strings.Contains(err.Error(), "activated after payload compaction") {
			t.Fatalf("error = %v, want post-compaction activation refusal", err)
		}
	})

	t.Run("rejection", func(t *testing.T) {
		ic := newCompactedContext(t, map[string]any{
			"status": "rejected", "latest_avatar_version": int64(1),
			"revision": int64(2),
		})
		feedAvatarArchiveRow(t, ic, "agent_avatar_rejections", map[string]any{
			"id": "avrej_aaaaaaaaaaaaaaaa", "account_id": avatarArchiveAccount,
			"realm_id": avatarArchiveRealm, "agent_id": avatarArchiveAgent,
			"avatar_version": int64(1), "reason_code": "operator_declined",
			"rejected_by_kind": PrincipalOperator, "rejected_by_id": avatarArchiveOperator,
			"rejected_at": "2026-07-17T12:20:00Z",
		})
		err := ic.validateImportedAvatarGraph()
		if err == nil || !strings.Contains(err.Error(), "rejected after payload compaction") {
			t.Fatalf("error = %v, want post-compaction rejection refusal", err)
		}
	})

	t.Run("reset protected pointer", func(t *testing.T) {
		ic := newCompactedContext(t, map[string]any{
			"status": "generation_due", "lineage_generation": int64(2),
			"latest_avatar_version": int64(1), "revision": int64(2),
		})
		feedAvatarArchiveReset(t, ic, map[string]any{
			"retired_proposed_version": int64(1),
			"reset_at":                 "2026-07-17T12:20:00Z",
		})
		err := ic.validateImportedAvatarGraph()
		if err == nil || !strings.Contains(err.Error(), "remained protected until after its reset boundary") {
			t.Fatalf("error = %v, want pre-reset compaction refusal", err)
		}
	})
}

func TestAvatarArchiveValidationEnforcesActiveParentLineage(t *testing.T) {
	t.Run("pending evolution cannot omit active parent", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "proposed", "latest_avatar_version": int64(2),
			"proposed_avatar_version": int64(2), "active_avatar_version": int64(1),
			"revision": int64(4),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 2, 0, avatardomain.SubjectHuman))
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})

		err := ic.validateImportedAvatarGraph()
		if err == nil || (!strings.Contains(err.Error(), "parent does not match the active avatar") &&
			!strings.Contains(err.Error(), "omits a parent after activation")) {
			t.Fatalf("error = %v, want missing active-parent refusal", err)
		}
	})

	t.Run("pending evolution cannot name stale active parent", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "proposed", "latest_avatar_version": int64(3),
			"proposed_avatar_version": int64(3), "active_avatar_version": int64(2),
			"revision": int64(6),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman))
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 3, 1, avatardomain.SubjectHuman))
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_bbbbbbbbbbbbbbbb", "sequence": int64(2),
			"avatar_version": int64(2), "prior_active_version": int64(1),
			"action": "activated",
		})

		err := ic.validateImportedAvatarGraph()
		if err == nil || !strings.Contains(err.Error(), "parent does not match the active avatar") {
			t.Fatalf("error = %v, want stale active-parent refusal", err)
		}
	})

	t.Run("ordinary activation parent must equal prior active", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "active", "latest_avatar_version": int64(2),
			"active_avatar_version": int64(2), "revision": int64(4),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 2, 0, avatardomain.SubjectHuman))
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})
		row := avatarArchiveActivationRow(map[string]any{
			"id": "avact_bbbbbbbbbbbbbbbb", "sequence": int64(2),
			"avatar_version": int64(2), "prior_active_version": int64(1),
			"action": "activated",
		})
		err := ic.validateAndRecord("agent_avatar_activations", row)
		if err == nil || !strings.Contains(err.Error(), "parent disagrees with prior_active_version") {
			t.Fatalf("error = %v, want activation-parent refusal", err)
		}
	})

	t.Run("first ordinary activation cannot carry a parent", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "active", "latest_avatar_version": int64(2),
			"active_avatar_version": int64(2), "revision": int64(4),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		row := avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman)
		row["proposed_by_kind"] = PrincipalOperator
		row["proposed_by_id"] = avatarArchiveOperator
		feedAvatarArchiveVersion(t, ic, row)
		activation := avatarArchiveActivationRow(map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(2),
			"prior_active_version": nil, "action": "activated",
		})
		err := ic.validateAndRecord("agent_avatar_activations", activation)
		if err == nil || !strings.Contains(err.Error(), "first ordinary activation version carries a parent") {
			t.Fatalf("error = %v, want first-activation parent refusal", err)
		}
	})

	t.Run("post-activation rejected version cannot omit parent", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "active", "latest_avatar_version": int64(2),
			"active_avatar_version": int64(1), "revision": int64(5),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 2, 0, avatardomain.SubjectHuman))
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})
		feedAvatarArchiveRejection(t, ic, 2)

		err := ic.validateImportedAvatarGraph()
		if err == nil || !strings.Contains(err.Error(), "omits a parent after activation") {
			t.Fatalf("error = %v, want post-activation parent refusal", err)
		}
	})

	t.Run("parent must name an activated version", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "active", "latest_avatar_version": int64(3),
			"active_avatar_version": int64(1), "revision": int64(7),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman))
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 3, 2, avatardomain.SubjectHuman))
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})
		feedAvatarArchiveRejection(t, ic, 2)
		feedAvatarArchiveRejection(t, ic, 3)

		err := ic.validateImportedAvatarGraph()
		if err == nil || !strings.Contains(err.Error(), "parent that was never activated") {
			t.Fatalf("error = %v, want never-active parent refusal", err)
		}
	})
}

func TestAvatarArchiveValidationAcceptsParentLineageLifecycleScenarios(t *testing.T) {
	t.Run("rejected initial retry chain", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "proposed", "latest_avatar_version": int64(3),
			"proposed_avatar_version": int64(3), "revision": int64(6),
		})
		for version := int64(1); version <= 3; version++ {
			feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, version, 0, avatardomain.SubjectHuman))
		}
		feedAvatarArchiveRejection(t, ic, 1)
		feedAvatarArchiveRejection(t, ic, 2)

		if err := ic.validateImportedAvatarGraph(); err != nil {
			t.Fatalf("rejected initial retry chain = %v", err)
		}
	})

	t.Run("rollback followed by exact-parent proposal", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "proposed", "latest_avatar_version": int64(3),
			"proposed_avatar_version": int64(3), "active_avatar_version": int64(1),
			"revision": int64(8),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectHuman))
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 3, 1, avatardomain.SubjectHuman))
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_bbbbbbbbbbbbbbbb", "sequence": int64(2),
			"avatar_version": int64(2), "prior_active_version": int64(1),
			"action": "activated",
		})
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_cccccccccccccccc", "sequence": int64(3),
			"avatar_version": int64(1), "prior_active_version": int64(2),
			"action": "rolled_back",
		})

		if err := ic.validateImportedAvatarGraph(); err != nil {
			t.Fatalf("rollback lineage = %v", err)
		}
	})

	t.Run("style migration keeps exact parent while continuity is exempt", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		pack := avatardomain.BuiltInFlatVectorStylePack()
		pack.Version = 2
		styleV2 := avatarStyleVersionImportKey{
			realmID: avatarArchiveRealm, stylePackID: pack.ID, version: 2,
		}
		ic.avatarStyleVersions[styleV2] = avatarStyleVersionImportScope{pack: pack, previousVersion: 1}
		headKey := avatarStyleHeadImportKey{realmID: avatarArchiveRealm, stylePackID: pack.ID}
		head := ic.avatarStyleHeads[headKey]
		head.currentVersion = 2
		ic.avatarStyleHeads[headKey] = head
		selected := ic.realmAvatarStyles[avatarArchiveRealm]
		selected.style = styleV2
		selected.revision = 2
		ic.realmAvatarStyles[avatarArchiveRealm] = selected
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "active", "style_pack_version": int64(2),
			"latest_avatar_version": int64(2), "active_avatar_version": int64(2),
			"subject_form": "animal", "revision": int64(5),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		migration := avatarArchiveVersionRow(t, 2, 1, avatardomain.SubjectAnimal)
		migration["style_pack_version"] = int64(2)
		feedAvatarArchiveVersion(t, ic, migration)
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_bbbbbbbbbbbbbbbb", "sequence": int64(2),
			"avatar_version": int64(2), "prior_active_version": int64(1),
			"action": "activated",
		})

		if err := ic.validateImportedAvatarGraph(); err != nil {
			t.Fatalf("style migration lineage = %v", err)
		}
	})
}

func TestAvatarArchiveValidationRejectsRetainedPayloadUsageAboveQuota(t *testing.T) {
	tests := []struct {
		name       string
		versions   int64
		countLimit int64
		byteLimit  int64
		bytesEach  int64
	}{
		{name: "count", versions: 5, countLimit: 4,
			byteLimit: AvatarDefaultRetainedPayloadByteLimit},
		{name: "bytes", versions: 6, countLimit: 20,
			byteLimit: AvatarMinRetainedPayloadByteLimit, bytesEach: 100_000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ic := newAvatarArchiveImportContext(t)
			feedAvatarArchiveStyle(t, ic, false)
			feedAvatarArchiveProfile(t, ic, map[string]any{
				"status": "proposed", "latest_avatar_version": test.versions,
				"proposed_avatar_version":      test.versions,
				"retained_payload_count_limit": test.countLimit,
				"retained_payload_byte_limit":  test.byteLimit,
			})
			for version := int64(1); version <= test.versions; version++ {
				row := avatarArchiveVersionRow(t, version, 0, avatardomain.SubjectHuman)
				if test.bytesEach > 0 {
					row["payload_bytes"] = test.bytesEach
				}
				feedAvatarArchiveVersion(t, ic, row)
				if version < test.versions {
					feedAvatarArchiveRejection(t, ic, version)
				}
			}
			err := ic.validateImportedAvatarGraph()
			if err == nil || !strings.Contains(err.Error(), "exceed the configured quota") {
				t.Fatalf("error = %v, want retained quota refusal", err)
			}
		})
	}

	t.Run("continuity fingerprint bytes", func(t *testing.T) {
		pack := avatardomain.BuiltInFlatVectorStylePack()
		fingerprint, err := avatardomain.BuildPerceptualContinuityFingerprint(
			[]byte(pack.References[0].SVG), pack)
		if err != nil {
			t.Fatal(err)
		}
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "proposed", "latest_avatar_version": int64(5),
			"active_avatar_version": int64(1), "proposed_avatar_version": int64(5),
			"retained_payload_count_limit": int64(20),
			"retained_payload_byte_limit":  AvatarMinRetainedPayloadByteLimit,
		})
		feedAvatarArchiveVersion(t, ic, compactAvatarArchiveVersionRow(t, 1, 0, fingerprint))
		for version := int64(2); version <= 5; version++ {
			row := avatarArchiveVersionRow(t, version, 1, avatardomain.SubjectHuman)
			row["payload_bytes"] = int64(125_000)
			if version > 2 {
				row["proposed_by_kind"] = PrincipalOperator
				row["proposed_by_id"] = avatarArchiveOperator
			}
			feedAvatarArchiveVersion(t, ic, row)
		}
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})
		err = ic.validateImportedAvatarGraph()
		if err == nil || !strings.Contains(err.Error(), "exceed the configured quota") {
			t.Fatalf("error = %v, want inclusive fingerprint-byte quota refusal", err)
		}
	})
}

func TestAvatarArchiveValidationAcceptsBothBuiltInPersistedRepresentations(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "migration-backfill"}[legacy], func(t *testing.T) {
			ic := newAvatarArchiveImportContext(t)
			feedAvatarArchiveStyle(t, ic, legacy)
			if err := ic.validateImportedAvatarGraph(); err == nil || !strings.Contains(err.Error(), "no avatar profile") {
				// The style graph itself passed. The fixture deliberately has an
				// agent but no profile, so final validation must reach that later
				// invariant instead of rejecting the style representation.
				t.Fatalf("graph after accepted style = %v, want missing profile", err)
			}
		})
	}
}

func TestAvatarArchiveValidationRejectsHostileBuiltInPayload(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{
			name: "trusted id with replaced style spec",
			mutate: func(row map[string]any) {
				row["style_spec"] = map[string]any{"id": avatardomain.DefaultStylePackID, "version": float64(1)}
			},
			want: "not a recognized canonical representation",
		},
		{
			name: "trusted id with injected reference",
			mutate: func(row map[string]any) {
				row["reference_examples"] = []any{map[string]any{"svg": `<svg onload="steal()"/>`}}
			},
			want: "not a recognized canonical representation",
		},
		{
			name: "trusted id with foreign provenance",
			mutate: func(row map[string]any) {
				row["provenance"] = map[string]any{"source": "archive.attacker", "revision": "1"}
			},
			want: "provenance is not canonical",
		},
		{
			name: "trusted id claimed by operator",
			mutate: func(row map[string]any) {
				row["created_by_kind"] = PrincipalOperator
				row["created_by_id"] = avatarArchiveOperator
			},
			want: "must be system-authored",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ic := newAvatarArchiveImportContext(t)
			feedAvatarArchiveStyleHead(t, ic)
			row := avatarArchiveStyleVersionRow(t, false)
			tc.mutate(row)
			err := ic.validateAndRecord("avatar_style_pack_versions", row)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAvatarArchiveValidationRejectsHostileSVGHashAndProvenance(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{
			name: "scriptable svg with matching hash",
			mutate: func(row map[string]any) {
				svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512"><script>alert(1)</script></svg>`
				row["svg"] = svg
				digest := sha256.Sum256([]byte(svg))
				row["svg_sha256"] = hex.EncodeToString(digest[:])
			},
			want: "svg is not canonical and style-valid",
		},
		{
			name:   "digest substitution",
			mutate: func(row map[string]any) { row["svg_sha256"] = strings.Repeat("0", 64) },
			want:   "svg_sha256 mismatch",
		},
		{
			name: "unknown provenance field",
			mutate: func(row map[string]any) {
				row["provenance"].(map[string]any)["prompt"] = "hidden instructions"
			},
			want: "provenance contains unknown field",
		},
		{
			name:   "cross-realm proposer",
			mutate: func(row map[string]any) { row["proposed_by_id"] = "agent_from_other_realm" },
			want:   "proposer is outside the imported scope",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ic := newAvatarArchiveImportContext(t)
			feedAvatarArchiveStyle(t, ic, false)
			feedAvatarArchiveProfile(t, ic, map[string]any{})
			row := avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman)
			tc.mutate(row)
			err := ic.validateAndRecord("agent_avatar_versions", row)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAvatarArchiveValidationRejectsSameRealmPeerAsAvatarActor(t *testing.T) {
	const peer = "agent_avatar_archive_peer"
	for _, tc := range []struct {
		name  string
		table string
		row   func(*testing.T) map[string]any
		want  string
	}{
		{
			name: "proposal", table: "agent_avatar_versions",
			row: func(t *testing.T) map[string]any {
				row := avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman)
				row["proposed_by_id"] = peer
				return row
			},
			want: "does not own the avatar",
		},
		{
			name: "activation", table: "agent_avatar_activations",
			row: func(_ *testing.T) map[string]any {
				return avatarArchiveActivationRow(map[string]any{
					"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
					"prior_active_version": nil, "action": "activated", "activated_by_id": peer,
				})
			},
			want: "does not own the avatar",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ic := newAvatarArchiveImportContext(t)
			ic.agents[peer], ic.liveAgents[peer], ic.agentRealms[peer] = true, true, avatarArchiveRealm
			feedAvatarArchiveStyle(t, ic, false)
			feedAvatarArchiveProfile(t, ic, map[string]any{
				"status": "proposed", "latest_avatar_version": int64(1),
				"proposed_avatar_version": int64(1), "revision": int64(2),
			})
			if tc.table == "agent_avatar_activations" {
				feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
			}
			err := ic.validateAndRecord(tc.table, tc.row(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAvatarArchiveValidationRejectsSameRealmPeerReceiptActor(t *testing.T) {
	const peer = "agent_avatar_archive_peer"
	ic := newAvatarArchiveImportContext(t)
	ic.agents[peer], ic.liveAgents[peer], ic.agentRealms[peer] = true, true, avatarArchiveRealm
	feedAvatarArchiveStyle(t, ic, false)
	feedAvatarArchiveProfile(t, ic, map[string]any{})
	row := avatarArchiveReceiptRow("propose", PrincipalAgent, peer, int64(1))
	err := ic.validateAndRecord("avatar_mutation_receipts", row)
	if err == nil || !strings.Contains(err.Error(), "does not own the avatar") {
		t.Fatalf("error = %v, want peer receipt refusal", err)
	}
}

func TestAvatarArchiveValidationRejectsImpossibleLifecycleProjection(t *testing.T) {
	tests := []struct {
		name       string
		profile    map[string]any
		withActive bool
		live       bool
		want       string
	}{
		{"active without pointer", map[string]any{"status": "active"}, false, true, "requires an active avatar"},
		{"evolution without pointer", map[string]any{"status": "evolution_due"}, false, true, "requires an active avatar"},
		{"generation due with active", map[string]any{"status": "generation_due"}, true, true, "requires no active avatar"},
		{"rejected with active", map[string]any{"status": "rejected"}, true, true, "requires no active avatar"},
		{"archived live agent", map[string]any{"status": "archived"}, false, true, "requires a deleted agent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ic := newAvatarArchiveImportContext(t)
			ic.liveAgents[avatarArchiveAgent] = test.live
			feedAvatarArchiveStyle(t, ic, false)
			profile := map[string]any{}
			for key, value := range test.profile {
				profile[key] = value
			}
			if test.withActive {
				profile["latest_avatar_version"] = int64(1)
				profile["active_avatar_version"] = int64(1)
				profile["revision"] = int64(2)
			}
			feedAvatarArchiveProfile(t, ic, profile)
			if test.withActive {
				feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
				feedAvatarArchiveActivation(t, ic, map[string]any{
					"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
					"prior_active_version": nil, "action": "activated",
				})
			}
			err := ic.validateImportedAvatarGraph()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAvatarArchiveValidationBoundsGenerationFailureRetryAuthority(t *testing.T) {
	failedProfile := func(retryAfter time.Time) map[string]any {
		return avatarArchiveProfileRow(map[string]any{
			"status": "generation_failed", "attempt_count": int64(1),
			"failure_code": "renderer_unavailable",
			"retry_after":  retryAfter.UTC().Format(time.RFC3339Nano),
		})
	}

	t.Run("accepts exact manifest and live-backoff ceiling", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		if err := ic.validateAndRecord("agent_avatar_profiles",
			failedProfile(ic.exportedAt.Add(maxAvatarGenerationBackoff))); err != nil {
			t.Fatalf("exact retry ceiling = %v", err)
		}
	})

	t.Run("accepts only the declared cross-cell skew allowance", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		ic.exportedAt = ic.importedAt.Add(maxArchiveManifestFutureSkew)
		feedAvatarArchiveStyle(t, ic, false)
		if err := ic.validateAndRecord("agent_avatar_profiles",
			failedProfile(ic.importedAt.Add(maxAvatarGenerationBackoff+maxArchiveManifestFutureSkew))); err != nil {
			t.Fatalf("clock-skew retry ceiling = %v", err)
		}
	})

	t.Run("rejects retry beyond destination-owned ceiling", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		// Even an attacker-chosen far-future manifest cannot extend the
		// destination database clock ceiling.
		ic.exportedAt = ic.importedAt.Add(24 * time.Hour)
		feedAvatarArchiveStyle(t, ic, false)
		err := ic.validateAndRecord("agent_avatar_profiles", failedProfile(
			ic.importedAt.Add(maxAvatarGenerationBackoff+maxArchiveManifestFutureSkew+time.Nanosecond),
		))
		if err == nil || !strings.Contains(err.Error(), "exceeds destination import time") {
			t.Fatalf("error = %v, want retry ceiling refusal", err)
		}
	})

	t.Run("non-failed profile cannot carry retry authority", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		row := avatarArchiveProfileRow(map[string]any{
			"retry_after": ic.importedAt.Add(time.Minute).Format(time.RFC3339Nano),
		})
		err := ic.validateAndRecord("agent_avatar_profiles", row)
		if err == nil || !strings.Contains(err.Error(), "non-failed profile carries failure state") {
			t.Fatalf("error = %v, want non-failed retry refusal", err)
		}
	})
}

func TestAvatarArchiveValidationRejectsOffStyleActiveProjection(t *testing.T) {
	ic := newAvatarArchiveImportContext(t)
	feedAvatarArchiveStyle(t, ic, false)
	feedAvatarArchiveProfile(t, ic, map[string]any{
		"status": "active", "latest_avatar_version": int64(1),
		"active_avatar_version": int64(1), "revision": int64(2),
	})
	feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
	feedAvatarArchiveActivation(t, ic, map[string]any{
		"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
		"prior_active_version": nil, "action": "activated",
	})

	// Model a realm that selected a newer style while the active pointer still
	// names a historical version from the prior style. That projection must be
	// evolution_due; importing it as active would suppress the retry checkpoint.
	selected := avatarStyleVersionImportKey{
		realmID: avatarArchiveRealm, stylePackID: "new-team-style", version: 1,
	}
	ic.avatarStyleHeads[avatarStyleHeadImportKey{
		realmID: avatarArchiveRealm, stylePackID: selected.stylePackID,
	}] = avatarStyleHeadImportScope{currentVersion: 1, revision: 1}
	ic.avatarStyleVersions[selected] = avatarStyleVersionImportScope{}
	ic.realmAvatarStyles[avatarArchiveRealm] = realmAvatarStyleImportScope{style: selected, revision: 2}
	profile := ic.avatarProfiles[avatarArchiveAgent]
	profile.style = selected
	ic.avatarProfiles[avatarArchiveAgent] = profile

	err := ic.validateImportedAvatarGraph()
	if err == nil || !strings.Contains(err.Error(), "active avatar style does not match") {
		t.Fatalf("error = %v, want off-style active refusal", err)
	}
}

func TestAvatarArchiveValidationRejectsEveryCrossTenantAvatarRow(t *testing.T) {
	for _, table := range []string{
		"avatar_style_packs", "avatar_style_pack_versions", "realm_avatar_styles",
		"agent_avatar_profiles", "agent_avatar_versions", "agent_avatar_activations",
		"agent_avatar_rejections", "agent_avatar_resets", "avatar_mutation_receipts",
	} {
		t.Run(table, func(t *testing.T) {
			ic := newAvatarArchiveImportContext(t)
			err := ic.validateAndRecord(table, map[string]any{"account_id": "acc_other"})
			if err == nil || !strings.Contains(err.Error(), "does not match manifest") {
				t.Fatalf("error = %v, want account scope refusal", err)
			}
		})
	}
}

func TestAvatarArchiveValidationBindsLedgerAndReceiptSemantics(t *testing.T) {
	t.Run("ordinary activation cannot reactivate historical version", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "active", "latest_avatar_version": int64(1),
			"active_avatar_version": int64(1), "revision": int64(4),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})
		row := avatarArchiveActivationRow(map[string]any{
			"id": "avact_bbbbbbbbbbbbbbbb", "sequence": int64(2),
			"avatar_version":       int64(1),
			"prior_active_version": int64(1), "action": "activated",
		})
		err := ic.validateAndRecord("agent_avatar_activations", row)
		if err == nil || !strings.Contains(err.Error(), "without rollback semantics") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("activation sequence must be contiguous", func(t *testing.T) {
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "active", "latest_avatar_version": int64(1),
			"active_avatar_version": int64(1), "revision": int64(2),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		row := avatarArchiveActivationRow(map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "sequence": int64(2),
			"avatar_version": int64(1), "prior_active_version": nil,
			"action": "activated",
		})
		err := ic.validateAndRecord("agent_avatar_activations", row)
		if err == nil || !strings.Contains(err.Error(), "sequence is invalid") {
			t.Fatalf("error = %v, want activation sequence refusal", err)
		}
	})

	for _, tc := range []struct {
		name      string
		operation string
		actorKind string
		actorID   string
		version   any
		want      string
	}{
		{"agent cannot reject", "reject", PrincipalAgent, avatarArchiveAgent, int64(1), "requires an operator"},
		{"operator cannot report generation failure", "fail", PrincipalOperator, avatarArchiveOperator, nil, "requires its target agent"},
		{"policy receipt cannot claim result version", "set_policy", PrincipalOperator, avatarArchiveOperator, int64(1), "result_version is inconsistent"},
		{"proposal receipt requires result version", "propose", PrincipalAgent, avatarArchiveAgent, nil, "result_version is inconsistent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ic := newAvatarArchiveImportContext(t)
			feedAvatarArchiveStyle(t, ic, false)
			feedAvatarArchiveProfile(t, ic, map[string]any{})
			row := avatarArchiveReceiptRow(tc.operation, tc.actorKind, tc.actorID, tc.version)
			err := ic.validateAndRecord("avatar_mutation_receipts", row)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}

	newResetReceiptContext := func(t *testing.T) *importCtx {
		t.Helper()
		ic := newAvatarArchiveImportContext(t)
		feedAvatarArchiveStyle(t, ic, false)
		feedAvatarArchiveProfile(t, ic, map[string]any{
			"status": "generation_due", "lineage_generation": int64(2),
			"latest_avatar_version": int64(1), "revision": int64(4),
		})
		feedAvatarArchiveVersion(t, ic, avatarArchiveVersionRow(t, 1, 0, avatardomain.SubjectHuman))
		feedAvatarArchiveActivation(t, ic, map[string]any{
			"id": "avact_aaaaaaaaaaaaaaaa", "avatar_version": int64(1),
			"prior_active_version": nil, "action": "activated",
		})
		feedAvatarArchiveRow(t, ic, "agent_avatar_resets", avatarArchiveResetRow(map[string]any{
			"retired_active_version": int64(1),
		}))
		return ic
	}
	resetReceipt := func(actorKind, actorID, key string) map[string]any {
		row := avatarArchiveReceiptRow("reset", actorKind, actorID, nil)
		row["idempotency_key"] = key
		row["result_revision"] = int64(4)
		row["result_lineage_generation"] = int64(2)
		return row
	}

	t.Run("reset receipt actor must match lifecycle actor", func(t *testing.T) {
		ic := newResetReceiptContext(t)
		err := ic.validateAndRecord("avatar_mutation_receipts",
			resetReceipt(PrincipalOperator, avatarArchiveOperator, "forged-reset-actor"))
		if err == nil || !strings.Contains(err.Error(), "actor does not match") {
			t.Fatalf("error = %v, want reset actor binding refusal", err)
		}
	})

	t.Run("reset lifecycle accepts exactly one matching receipt", func(t *testing.T) {
		ic := newResetReceiptContext(t)
		feedAvatarArchiveRow(t, ic, "avatar_mutation_receipts",
			resetReceipt(PrincipalAgent, avatarArchiveAgent, "reset-receipt-1"))
		err := ic.validateAndRecord("avatar_mutation_receipts",
			resetReceipt(PrincipalAgent, avatarArchiveAgent, "reset-receipt-2"))
		if err == nil || !strings.Contains(err.Error(), "more than one receipt") {
			t.Fatalf("error = %v, want duplicate reset receipt refusal", err)
		}
		if err := ic.validateImportedAvatarGraph(); err != nil {
			t.Fatalf("matching reset receipt graph = %v", err)
		}
	})

	t.Run("reset lifecycle requires its receipt", func(t *testing.T) {
		ic := newResetReceiptContext(t)
		err := ic.validateImportedAvatarGraph()
		if err == nil || !strings.Contains(err.Error(), "reset has no receipt") {
			t.Fatalf("error = %v, want missing reset receipt refusal", err)
		}
	})
}

func newAvatarArchiveImportContext(t *testing.T) *importCtx {
	t.Helper()
	ic := newImportCtx(avatarArchiveAccount)
	ic.exportedAt = time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC)
	ic.importedAt = ic.exportedAt
	ic.realms[avatarArchiveRealm] = true
	ic.operators[avatarArchiveOperator] = true
	ic.agents[avatarArchiveAgent] = true
	ic.liveAgents[avatarArchiveAgent] = true
	ic.agentRealms[avatarArchiveAgent] = avatarArchiveRealm
	return ic
}

func feedAvatarArchiveStyle(t *testing.T, ic *importCtx, legacy bool) {
	t.Helper()
	feedAvatarArchiveStyleHead(t, ic)
	feedAvatarArchiveRow(t, ic, "avatar_style_pack_versions", avatarArchiveStyleVersionRow(t, legacy))
	feedAvatarArchiveRow(t, ic, "realm_avatar_styles", map[string]any{
		"account_id": avatarArchiveAccount, "realm_id": avatarArchiveRealm,
		"style_pack_id":      avatardomain.DefaultStylePackID,
		"style_pack_version": int64(1), "revision": int64(1),
		"created_at": avatarArchiveTime, "updated_at": avatarArchiveTime,
	})
}

func feedAvatarArchiveStyleHead(t *testing.T, ic *importCtx) {
	t.Helper()
	feedAvatarArchiveRow(t, ic, "avatar_style_packs", map[string]any{
		"account_id": avatarArchiveAccount, "realm_id": avatarArchiveRealm,
		"id": avatardomain.DefaultStylePackID, "current_version": int64(1),
		"revision": int64(1), "created_at": avatarArchiveTime, "updated_at": avatarArchiveTime,
	})
}

func avatarArchiveStyleVersionRow(t *testing.T, legacy bool) map[string]any {
	t.Helper()
	pack := avatardomain.BuiltInFlatVectorStylePack()
	styleSpec := avatarArchiveJSONValue(t, pack).(map[string]any)
	references := avatarArchiveJSONValue(t, pack.References).([]any)
	description := pack.Description
	if legacy {
		styleSpec = avatarArchiveJSONRaw(t, importedLegacyBuiltInAvatarStyleSpec).(map[string]any)
		references = []any{}
		description = importedLegacyBuiltInAvatarDescription
	}
	return map[string]any{
		"account_id": avatarArchiveAccount, "realm_id": avatarArchiveRealm,
		"style_pack_id": pack.ID, "version": int64(1), "previous_version": nil,
		"name": pack.Name, "description": description, "style_spec": styleSpec,
		"reference_examples": references,
		"provenance":         map[string]any{"source": "witself.builtin", "revision": "1"},
		"created_by_kind":    ActorSystem, "created_by_id": "", "created_at": avatarArchiveTime,
	}
}

func feedAvatarArchiveProfile(t *testing.T, ic *importCtx, overrides map[string]any) {
	t.Helper()
	feedAvatarArchiveRow(t, ic, "agent_avatar_profiles", avatarArchiveProfileRow(overrides))
}

func avatarArchiveProfileRow(overrides map[string]any) map[string]any {
	row := map[string]any{
		"account_id": avatarArchiveAccount, "realm_id": avatarArchiveRealm,
		"agent_id": avatarArchiveAgent, "status": "generation_due",
		"lineage_generation": int64(1),
		"autonomy_policy":    "agent_self_managed",
		"style_pack_id":      avatardomain.DefaultStylePackID, "style_pack_version": int64(1),
		"latest_avatar_version": nil, "proposed_avatar_version": nil,
		"active_avatar_version": nil, "subject_form": "human",
		"attempt_count": int64(0), "retry_after": nil, "fallback_seed": avatarArchiveAgent,
		"failure_code": "", "revision": int64(1),
		"retained_payload_count_limit": int64(AvatarDefaultRetainedPayloadCountLimit),
		"retained_payload_byte_limit":  AvatarDefaultRetainedPayloadByteLimit,
		"created_at":                   avatarArchiveTime, "updated_at": avatarArchiveTime,
	}
	for key, value := range overrides {
		row[key] = value
	}
	return row
}

func avatarArchiveVersionRow(t *testing.T, version, parent int64, form avatardomain.SubjectForm) map[string]any {
	t.Helper()
	pack := avatardomain.BuiltInFlatVectorStylePack()
	reference := pack.References[0]
	for _, candidate := range pack.References {
		if candidate.SubjectForm == form {
			reference = candidate
			break
		}
	}
	digest := sha256.Sum256([]byte(reference.SVG))
	lockedDigest, err := avatardomain.LockedLayersSHA256([]byte(reference.SVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	var parentValue any
	if parent > 0 {
		parentValue = parent
	}
	description := "A calm team portrait in the shared flat vector style."
	visualSpec := map[string]any{"identity": map[string]any{"expression": "calm"}}
	rawSpec, err := json.Marshal(visualSpec)
	if err != nil {
		t.Fatal(err)
	}
	payloadBytes, err := avatarCreativePayloadBytes(reference.SVG, description, rawSpec)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"account_id": avatarArchiveAccount, "realm_id": avatarArchiveRealm,
		"agent_id": avatarArchiveAgent, "id": avatarArchiveVersionID(version),
		"version": version, "lineage_generation": int64(1),
		"parent_version": parentValue,
		"style_pack_id":  pack.ID, "style_pack_version": int64(pack.Version),
		"subject_form": string(form), "svg": reference.SVG,
		"description":            description,
		"visual_spec":            visualSpec,
		"svg_sha256":             hex.EncodeToString(digest[:]),
		"locked_layers_sha256":   lockedDigest,
		"continuity_fingerprint": nil,
		"provenance":             map[string]any{"runtime": "cursor", "model": "GPT-5.6 Sol", "recipe": "avatar", "recipe_version": "1"},
		"proposed_by_kind":       PrincipalAgent, "proposed_by_id": avatarArchiveAgent,
		"proposed_at":          avatarArchiveTime,
		"payload_state":        string(avatardomain.PayloadFull),
		"payload_bytes":        payloadBytes,
		"payload_compacted_at": nil, "payload_compaction_reason": nil,
	}
}

func avatarArchiveContinuityFingerprint(t *testing.T, svg string) string {
	t.Helper()
	fingerprint, err := avatardomain.BuildPerceptualContinuityFingerprint(
		[]byte(svg), avatardomain.BuiltInFlatVectorStylePack())
	if err != nil {
		t.Fatal(err)
	}
	if len(fingerprint) != avatardomain.PerceptualContinuityFingerprintBytes {
		t.Fatalf("fingerprint length = %d, want %d", len(fingerprint),
			avatardomain.PerceptualContinuityFingerprintBytes)
	}
	return `\x` + hex.EncodeToString(fingerprint)
}

func compactAvatarArchiveVersionRow(t *testing.T, version, parent int64, fingerprint []byte) map[string]any {
	t.Helper()
	row := avatarArchiveVersionRow(t, version, parent, avatardomain.SubjectHuman)
	row["payload_state"] = string(avatardomain.PayloadCompacted)
	row["svg"], row["description"], row["visual_spec"] = nil, nil, nil
	row["payload_compacted_at"] = "2026-07-17T12:30:00Z"
	row["payload_compaction_reason"] = "quota"
	if len(fingerprint) > 0 {
		row["continuity_fingerprint"] = `\x` + hex.EncodeToString(fingerprint)
	}
	return row
}

func setAvatarArchiveVersionSVG(t *testing.T, row map[string]any, svg string) {
	t.Helper()
	row["svg"] = svg
	digest := sha256.Sum256([]byte(svg))
	row["svg_sha256"] = hex.EncodeToString(digest[:])
	rawSpec, err := json.Marshal(row["visual_spec"])
	if err != nil {
		t.Fatal(err)
	}
	payloadBytes, err := avatarCreativePayloadBytes(svg, row["description"].(string), rawSpec)
	if err != nil {
		t.Fatal(err)
	}
	row["payload_bytes"] = payloadBytes
}

func avatarArchiveVersionID(version int64) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	if version < 1 || version > int64(len(alphabet)) {
		panic("avatar archive test version id is out of range")
	}
	return "avver_" + strings.Repeat(string(alphabet[version-1]), 16)
}

func feedAvatarArchiveVersion(t *testing.T, ic *importCtx, row map[string]any) {
	t.Helper()
	feedAvatarArchiveRow(t, ic, "agent_avatar_versions", row)
}

func avatarArchiveActivationRow(overrides map[string]any) map[string]any {
	row := map[string]any{
		"account_id": avatarArchiveAccount, "realm_id": avatarArchiveRealm,
		"agent_id": avatarArchiveAgent, "sequence": int64(1),
		"lineage_generation": int64(1),
		"activated_by_kind":  PrincipalAgent,
		"activated_by_id":    avatarArchiveAgent, "activated_at": avatarArchiveTime,
	}
	for key, value := range overrides {
		row[key] = value
	}
	return row
}

func feedAvatarArchiveActivation(t *testing.T, ic *importCtx, overrides map[string]any) {
	t.Helper()
	feedAvatarArchiveRow(t, ic, "agent_avatar_activations", avatarArchiveActivationRow(overrides))
}

func feedAvatarArchiveRejection(t *testing.T, ic *importCtx, version int64) {
	t.Helper()
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	if version < 1 || version > int64(len(alphabet)) {
		t.Fatal("avatar archive test rejection id is out of range")
	}
	feedAvatarArchiveRow(t, ic, "agent_avatar_rejections", map[string]any{
		"id":         "avrej_" + strings.Repeat(string(alphabet[version-1]), 16),
		"account_id": avatarArchiveAccount, "realm_id": avatarArchiveRealm,
		"agent_id": avatarArchiveAgent, "avatar_version": version,
		"reason_code": "operator_declined", "rejected_by_kind": PrincipalOperator,
		"rejected_by_id": avatarArchiveOperator, "rejected_at": avatarArchiveTime,
	})
}

func avatarArchiveResetRow(overrides map[string]any) map[string]any {
	row := map[string]any{
		"id": "avrst_aaaaaaaaaaaaaaaa", "account_id": avatarArchiveAccount,
		"realm_id": avatarArchiveRealm, "agent_id": avatarArchiveAgent,
		"sequence": int64(1), "retired_lineage_generation": int64(1),
		"new_lineage_generation": int64(2), "retired_active_version": nil,
		"retired_proposed_version": nil, "reason_code": "start_over",
		"reset_by_kind": PrincipalAgent, "reset_by_id": avatarArchiveAgent,
		"reset_at": avatarArchiveTime,
	}
	for key, value := range overrides {
		row[key] = value
	}
	return row
}

func feedAvatarArchiveReset(t *testing.T, ic *importCtx, overrides map[string]any) {
	t.Helper()
	reset := avatarArchiveResetRow(overrides)
	feedAvatarArchiveRow(t, ic, "agent_avatar_resets", reset)
	receipt := avatarArchiveReceiptRow("reset", reset["reset_by_kind"].(string),
		reset["reset_by_id"].(string), nil)
	receipt["result_revision"] = ic.avatarProfiles[avatarArchiveAgent].revision
	receipt["result_lineage_generation"] = reset["new_lineage_generation"]
	feedAvatarArchiveRow(t, ic, "avatar_mutation_receipts", receipt)
}

func avatarArchiveReceiptRow(operation, actorKind, actorID string, resultVersion any) map[string]any {
	return map[string]any{
		"account_id": avatarArchiveAccount, "realm_id": avatarArchiveRealm,
		"target_kind": "avatar", "target_id": avatarArchiveAgent,
		"actor_kind": actorKind, "actor_id": actorID, "operation": operation,
		"idempotency_key": "avatar-archive-receipt", "request_hash": strings.Repeat("a", 64),
		"result_revision": int64(2), "result_version": resultVersion,
		"result_lineage_generation": nil, "created_at": avatarArchiveTime,
	}
}

func feedAvatarArchiveRow(t *testing.T, ic *importCtx, table string, row map[string]any) {
	t.Helper()
	if err := ic.validateAndRecord(table, row); err != nil {
		t.Fatalf("%s fixture row = %v", table, err)
	}
}

func avatarArchiveJSONValue(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return avatarArchiveJSONRaw(t, raw)
}

func avatarArchiveJSONRaw(t *testing.T, raw []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
