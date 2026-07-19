package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	avatardomain "github.com/witwave-ai/witself/internal/avatar"
)

// TestAvatarCrossVersionLegacyReceiptReplayPostgres emulates canonical
// receipts and resources committed by the previous server immediately before
// an in-place upgrade. Exact retries must replay before perceptual-v1 gates;
// the same legacy-shaped request under a fresh key must still be rejected.
func TestAvatarCrossVersionLegacyReceiptReplayPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx,
		fmt.Sprintf("avatar-cross-version-%d@witwave.ai", time.Now().UnixNano()),
		"avatar cross-version replay", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := deleteAccountForIntegrationTest(context.Background(), st, provisioned.AccountID); err != nil {
			t.Errorf("delete integration account: %v", err)
		}
	}()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate account = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "cross-version")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "cross-version-avatar")
	if err != nil {
		t.Fatal(err)
	}
	agentPrincipal := Principal{Kind: PrincipalAgent, ID: agent.ID,
		AccountID: provisioned.AccountID, RealmID: realm.ID, AgentName: agent.Name,
		AccountStatus: "active"}
	operator := Principal{Kind: PrincipalOperator, ID: provisioned.OperatorID,
		AccountID: provisioned.AccountID, AccountStatus: "active"}
	style, err := st.GetRealmAvatarStyle(ctx, agentPrincipal, "")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("proposal receipt", func(t *testing.T) {
		strictSVG := style.StylePack.References[0].SVG
		legacySVG := addLegacyIdentityTransform(t, strictSVG)
		canonicalLegacySVG, err := avatardomain.SanitizeSVGForStylePack(
			[]byte(legacySVG), style.StylePack)
		if err != nil {
			t.Fatalf("generic legacy SVG validation: %v", err)
		}
		if _, err := avatardomain.SanitizePerceptualV1AvatarBaseline(
			canonicalLegacySVG, style.StylePack); err == nil {
			t.Fatal("perceptual-v1 unexpectedly accepted legacy transform")
		}

		proposal := ProposeAvatarInput{
			ExpectedProfileRevision: 1,
			StylePackID:             style.StylePack.ID,
			StylePackVersion:        style.StylePack.Version,
			SubjectForm:             avatardomain.SubjectHuman,
			Description:             "A legacy portrait committed immediately before an upgrade.",
			VisualSpec:              []byte(`{"identity":{"expression":"calm"}}`),
			SVG:                     strictSVG,
			Provenance: AvatarClientProvenance{Runtime: "legacy-runtime",
				Model: "legacy-model", Recipe: "avatar-initial", RecipeVersion: "185"},
			IdempotencyKey: "cross-version-legacy-proposal",
		}
		created, err := st.ProposeAvatar(ctx, agentPrincipal, proposal)
		if err != nil {
			t.Fatal(err)
		}
		if created.Receipt.Replayed || created.Receipt.ResultVersion != 1 {
			t.Fatalf("initial proposal receipt = %#v", created.Receipt)
		}

		description, err := avatardomain.NormalizeDescription(proposal.Description)
		if err != nil {
			t.Fatal(err)
		}
		visualSpec, err := avatardomain.NormalizeSpecJSON(proposal.VisualSpec)
		if err != nil {
			t.Fatal(err)
		}
		provenance, err := normalizeAvatarClient(proposal.Provenance)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(canonicalLegacySVG)
		svgSHA256 := hex.EncodeToString(digest[:])
		lockedLayersSHA256, err := avatardomain.LockedLayersSHA256(
			canonicalLegacySVG, style.StylePack)
		if err != nil {
			t.Fatal(err)
		}
		payloadBytes, err := avatarCreativePayloadBytes(
			string(canonicalLegacySVG), description, visualSpec)
		if err != nil {
			t.Fatal(err)
		}
		requestHash, err := avatarFingerprint(struct {
			ExpectedRevision int64                    `json:"expected_revision"`
			ParentVersion    int64                    `json:"parent_version"`
			StylePackID      string                   `json:"style_pack_id"`
			StyleVersion     int                      `json:"style_version"`
			SubjectForm      avatardomain.SubjectForm `json:"subject_form"`
			Description      string                   `json:"description"`
			VisualSpec       json.RawMessage          `json:"visual_spec"`
			SVGSHA256        string                   `json:"svg_sha256"`
			Provenance       AvatarClientProvenance   `json:"provenance"`
		}{proposal.ExpectedProfileRevision, proposal.ParentVersion,
			style.StylePack.ID, style.StylePack.Version, proposal.SubjectForm,
			description, visualSpec, svgSHA256, provenance})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE agent_avatar_versions
			   SET svg=$5, svg_sha256=$6, locked_layers_sha256=$7,
			       renderer_profile='legacy', payload_bytes=$8
			 WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3 AND version=$4`,
			provisioned.AccountID, realm.ID, agent.ID, int64(1),
			string(canonicalLegacySVG), svgSHA256, lockedLayersSHA256,
			payloadBytes); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE avatar_mutation_receipts SET request_hash=$9
			 WHERE account_id=$1 AND realm_id=$2
			   AND target_kind='avatar' AND target_id=$3
			   AND actor_kind=$4 AND actor_id=$5 AND operation='propose'
			   AND idempotency_key=$6 AND result_revision=$7 AND result_version=$8`,
			provisioned.AccountID, realm.ID, agent.ID, agentPrincipal.Kind,
			agentPrincipal.ID, proposal.IdempotencyKey,
			created.Receipt.ResultRevision, created.Receipt.ResultVersion,
			requestHash); err != nil {
			t.Fatal(err)
		}

		proposal.SVG = legacySVG
		replayed, err := st.ProposeAvatar(ctx, agentPrincipal, proposal)
		if err != nil {
			t.Fatalf("legacy proposal replay after upgrade: %v", err)
		}
		if !replayed.Receipt.Replayed || replayed.Receipt.ResultVersion != 1 ||
			replayed.Avatar.Proposed == nil ||
			replayed.Avatar.Proposed.RendererProfile != avatardomain.RendererProfileLegacy ||
			replayed.Avatar.Proposed.SVG != string(canonicalLegacySVG) {
			t.Fatalf("legacy proposal replay = %#v / %#v",
				replayed.Receipt, replayed.Avatar.Proposed)
		}

		proposal.IdempotencyKey = "cross-version-new-legacy-proposal"
		if _, err := st.ProposeAvatar(ctx, agentPrincipal, proposal); !errors.Is(err, ErrAvatarInputInvalid) {
			t.Fatalf("new legacy proposal = %v, want invalid input", err)
		}
	})

	t.Run("style receipt", func(t *testing.T) {
		strictPack := avatardomain.BuiltInFlatVectorStylePack()
		strictPack.Version = 2
		strictPack.Description = "A strict style committed immediately before an upgrade."
		styleInput := CreateAvatarStyleVersionInput{
			ExpectedStyleRevision: 1,
			StylePack:             strictPack,
			IdempotencyKey:        "cross-version-legacy-style",
		}
		created, err := st.SetRealmAvatarStyle(ctx, operator, realm.ID, styleInput)
		if err != nil {
			t.Fatal(err)
		}
		if created.Receipt.Replayed || created.Receipt.ResultRevision != 2 ||
			created.Receipt.ResultVersion != 2 {
			t.Fatalf("initial style receipt = %#v", created.Receipt)
		}

		legacyPack := strictPack
		legacyPack.References = append([]avatardomain.StyleReference(nil),
			strictPack.References...)
		for index := range legacyPack.References {
			legacyPack.References[index].SVG = addLegacyIdentityTransform(
				t, legacyPack.References[index].SVG)
			digest := sha256.Sum256([]byte(legacyPack.References[index].SVG))
			legacyPack.References[index].SHA256 = hex.EncodeToString(digest[:])
		}
		if err := legacyPack.Validate(); err != nil {
			t.Fatalf("generic legacy style validation: %v", err)
		}
		if err := avatardomain.ValidatePerceptualV1StylePack(legacyPack); err == nil {
			t.Fatal("perceptual-v1 unexpectedly accepted legacy style transforms")
		}

		packJSON, err := json.Marshal(legacyPack)
		if err != nil {
			t.Fatal(err)
		}
		referencesJSON, err := json.Marshal(legacyPack.References)
		if err != nil {
			t.Fatal(err)
		}
		legacyInput := CreateAvatarStyleVersionInput{
			ExpectedStyleRevision: 1,
			StylePack:             legacyPack,
			IdempotencyKey:        styleInput.IdempotencyKey,
		}
		fingerprintInput := legacyInput
		fingerprintInput.IdempotencyKey = ""
		requestHash, err := avatarFingerprint(fingerprintInput)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE avatar_style_pack_versions
			   SET style_spec=$5, reference_examples=$6
			 WHERE account_id=$1 AND realm_id=$2
			   AND style_pack_id=$3 AND version=$4`,
			provisioned.AccountID, realm.ID, legacyPack.ID, legacyPack.Version,
			packJSON, referencesJSON); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE avatar_mutation_receipts SET request_hash=$9
			 WHERE account_id=$1 AND realm_id=$2
			   AND target_kind='style_pack' AND target_id=$3
			   AND actor_kind=$4 AND actor_id=$5 AND operation='set_style'
			   AND idempotency_key=$6 AND result_revision=$7 AND result_version=$8`,
			provisioned.AccountID, realm.ID, realm.ID, operator.Kind, operator.ID,
			styleInput.IdempotencyKey, created.Receipt.ResultRevision,
			created.Receipt.ResultVersion, requestHash); err != nil {
			t.Fatal(err)
		}

		replayed, err := st.SetRealmAvatarStyle(ctx, operator, realm.ID, legacyInput)
		if err != nil {
			t.Fatalf("legacy style replay after upgrade: %v", err)
		}
		if !replayed.Receipt.Replayed || replayed.Style.StyleRevision != 2 ||
			replayed.Style.StylePack.Version != 2 ||
			replayed.Style.StylePack.References[0].SVG != legacyPack.References[0].SVG {
			t.Fatalf("legacy style replay = %#v / %#v",
				replayed.Receipt, replayed.Style)
		}

		legacyInput.ExpectedStyleRevision = 2
		legacyInput.IdempotencyKey = "cross-version-new-legacy-style"
		if _, err := st.SetRealmAvatarStyle(ctx, operator, realm.ID, legacyInput); !errors.Is(err, ErrAvatarInputInvalid) {
			t.Fatalf("new legacy style = %v, want invalid input", err)
		}
	})
}

func addLegacyIdentityTransform(t *testing.T, svg string) string {
	t.Helper()
	const original = `<g id="experience" data-layer="experience">`
	const replacement = `<g id="experience" data-layer="experience" transform="translate(0 0)">`
	transformed := strings.Replace(svg, original, replacement, 1)
	if transformed == svg {
		t.Fatal("test SVG does not contain the expected experience layer")
	}
	return transformed
}
