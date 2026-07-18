package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	avatardomain "github.com/witwave-ai/witself/internal/avatar"
	"github.com/witwave-ai/witself/internal/client"
)

func TestSelfCardJSONUsesIdentityOnlySelfAndOmitsCreativePayloads(t *testing.T) {
	const (
		factCanary     = "private-fact-canary"
		memoryCanary   = "private-memory-canary"
		svgCanary      = "svg-payload-canary"
		specCanary     = "visual-spec-canary"
		proposalCanary = "pending-proposal-canary"
	)
	identity, view := testSelfCardFixture(t, svgCanary)
	view.Active.VisualSpec = json.RawMessage(`{"private":"` + specCanary + `"}`)
	proposed := *view.Active
	proposed.Version = 2
	proposed.IsActive = false
	proposed.IsProposed = true
	proposed.Description = proposalCanary
	view.Proposed = &proposed
	view.Profile.ProposedVersion = proposed.Version
	view.Profile.Status = avatardomain.StatusProposed

	selfCalls := 0
	avatarCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/self":
			selfCalls++
			for _, key := range []string{
				"include_facts", "include_salient", "include_counts", "include_checkpoint",
				"include_message_checkpoint", "include_avatar_checkpoint", "include_sensitive",
			} {
				if got := r.URL.Query().Get(key); got != "false" {
					t.Errorf("%s = %q, want false", key, got)
				}
			}
			_ = json.NewEncoder(w).Encode(client.SelfDigest{
				SchemaVersion: "witself.v0", Identity: identity,
				PrimaryFacts:    []client.SelfFact{{ID: "fact_1", Name: "private", Value: factCanary}},
				SalientMemories: []client.SelfMemory{{ID: "mem_1", Snippet: memoryCanary}},
			})
		case "/v1/self/avatar":
			avatarCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"avatar": view})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{
			"self", "card", "--endpoint", srv.URL, "--token-file", tokenFile,
			"--realm", identity.RealmName, "--agent", identity.AgentName, "--json",
		})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("self card JSON = %d / %q / %q", code, stdout, stderr)
	}
	if selfCalls != 1 || avatarCalls != 1 {
		t.Fatalf("calls = self:%d avatar:%d", selfCalls, avatarCalls)
	}
	var doc selfCardDocument
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode self card: %v; output=%q", err, stdout)
	}
	if doc.SchemaVersion != selfCardSchemaVersion || doc.Identity.AgentID != identity.AgentID ||
		doc.Avatar.Kind != "active" || doc.Avatar.Version != 1 || !doc.Avatar.PendingUpdate {
		t.Fatalf("self card = %#v", doc)
	}
	if selfCardJSONHasForbiddenFields(doc) {
		t.Fatalf("self card JSON contains a forbidden content field: %s", stdout)
	}
	for _, canary := range []string{factCanary, memoryCanary, svgCanary, specCanary, proposalCanary} {
		if strings.Contains(stdout+stderr, canary) {
			t.Errorf("self card exposed %q: %q / %q", canary, stdout, stderr)
		}
	}
}

func TestValidateSelfCardRejectsIdentitySVGAndLifecycleInconsistency(t *testing.T) {
	identity, _ := testSelfCardFixture(t, "baseline")
	tests := []struct {
		name string
		edit func(*client.AvatarView)
		want string
	}{
		{
			name: "profile identity",
			edit: func(view *client.AvatarView) { view.Profile.AgentID = "agent_other" },
			want: "profile identity",
		},
		{
			name: "active identity",
			edit: func(view *client.AvatarView) { view.Active.RealmID = "realm_other" },
			want: "active avatar identity",
		},
		{
			name: "hash",
			edit: func(view *client.AvatarView) { view.Active.SVGSHA256 = strings.Repeat("0", 64) },
			want: "hash does not match",
		},
		{
			name: "unsafe SVG",
			edit: func(view *client.AvatarView) {
				view.Active.SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512"><script>bad()</script></svg>`
				sum := sha256.Sum256([]byte(view.Active.SVG))
				view.Active.SVGSHA256 = hex.EncodeToString(sum[:])
			},
			want: "validate active avatar SVG",
		},
		{
			name: "pointer",
			edit: func(view *client.AvatarView) { view.Profile.ActiveVersion = 4 },
			want: "version does not match",
		},
		{
			name: "proposed masquerading as active",
			edit: func(view *client.AvatarView) { view.Active.IsProposed = true },
			want: "one safe active presentation",
		},
		{
			name: "missing activation flag",
			edit: func(view *client.AvatarView) { view.Active.WasActivated = false },
			want: "no activation evidence",
		},
		{
			name: "missing activation timestamp",
			edit: func(view *client.AvatarView) { view.Active.LastActivatedAt = nil },
			want: "no activation evidence",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, view := testSelfCardFixture(t, "baseline")
			tt.edit(view)
			_, err := validateSelfCard(identity, view)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateSelfCard error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateSelfCardTreatsMissingMixedRolloutRendererProfileAsLegacy(t *testing.T) {
	identity, view := testSelfCardFixture(t, "mixed-rollout-renderer")
	view.Active.RendererProfile = ""
	validated, err := validateSelfCard(identity, view)
	if err != nil {
		t.Fatal(err)
	}
	if view.Active.RendererProfile != avatardomain.RendererProfileLegacy ||
		validated.Document.Avatar.RendererProfile != string(avatardomain.RendererProfileLegacy) {
		t.Fatalf("mixed-rollout renderer profile = view:%q card:%q",
			view.Active.RendererProfile, validated.Document.Avatar.RendererProfile)
	}
}

func TestValidateSelfCardAcceptsDeterministicPlaceholder(t *testing.T) {
	identity, view := testSelfCardFixture(t, "placeholder")
	view.Profile.ActiveVersion = 0
	view.Profile.Status = avatardomain.StatusGenerationDue
	view.Active.ID = "placeholder-0123456789abcdef"
	view.Active.Version = 0
	view.Active.ProposedBy = client.AvatarActor{Kind: "system"}
	view.Active.WasActivated = false
	view.Active.LastActivatedAt = nil
	card, err := validateSelfCard(identity, view)
	if err != nil {
		t.Fatal(err)
	}
	if card.Document.Avatar.Kind != "placeholder" || card.Document.Avatar.Version != 0 {
		t.Fatalf("placeholder card = %#v", card.Document.Avatar)
	}

	// An initial operator-governed proposal may change the profile's target
	// subject while the human placeholder remains the active presentation.
	view.Profile.Status = avatardomain.StatusProposed
	view.Profile.SubjectForm = avatardomain.SubjectAnimal
	view.Profile.ProposedVersion = 2
	card, err = validateSelfCard(identity, view)
	if err != nil {
		t.Fatal(err)
	}
	if card.Document.Avatar.SubjectForm != string(avatardomain.SubjectHuman) || !card.Document.Avatar.PendingUpdate {
		t.Fatalf("pending placeholder card = %#v", card.Document.Avatar)
	}
}

func TestValidateSelfCardRejectsInconsistentPlaceholderArtifact(t *testing.T) {
	identity, view := testSelfCardFixture(t, "placeholder-integrity")
	view.Profile.ActiveVersion = 0
	view.Profile.Status = avatardomain.StatusGenerationDue
	view.Active.ID = "placeholder-0123456789abcdef"
	view.Active.Version = 0
	view.Active.ProposedBy = client.AvatarActor{Kind: "system"}
	view.Active.WasActivated = false
	view.Active.LastActivatedAt = nil

	t.Run("non-human", func(t *testing.T) {
		copyView := *view
		copyActive := *view.Active
		copyView.Active = &copyActive
		copyView.Active.SubjectForm = avatardomain.SubjectAnimal
		_, err := validateSelfCard(identity, &copyView)
		if err == nil || !strings.Contains(err.Error(), "placeholder metadata") {
			t.Fatalf("validateSelfCard error = %v", err)
		}
	})

	t.Run("wrong style", func(t *testing.T) {
		copyView := *view
		copyActive := *view.Active
		copyView.Active = &copyActive
		copyView.Active.Style.StylePackID = "other-style"
		_, err := validateSelfCard(identity, &copyView)
		if err == nil || !strings.Contains(err.Error(), "placeholder metadata") {
			t.Fatalf("validateSelfCard error = %v", err)
		}
	})
}

func TestValidateSelfCardUsesActiveArtifactDuringProfileTransitions(t *testing.T) {
	t.Run("realm style target changed", func(t *testing.T) {
		identity, view := testSelfCardFixture(t, "style-transition")
		view.Profile.Status = avatardomain.StatusEvolutionDue
		view.Profile.Style.StylePackID = "next-flat-style"
		view.Profile.Style.Version = 2
		card, err := validateSelfCard(identity, view)
		if err != nil {
			t.Fatal(err)
		}
		if card.Document.Avatar.Style.ID != avatardomain.DefaultStylePackID ||
			card.Document.Avatar.Style.Version != avatardomain.BuiltInStylePackVersion {
			t.Fatalf("card did not retain active style: %#v", card.Document.Avatar.Style)
		}
	})

	t.Run("pending proposal changed subject target", func(t *testing.T) {
		identity, view := testSelfCardFixture(t, "subject-transition")
		view.Profile.Status = avatardomain.StatusProposed
		view.Profile.SubjectForm = avatardomain.SubjectAnimal
		view.Profile.ProposedVersion = 2
		card, err := validateSelfCard(identity, view)
		if err != nil {
			t.Fatal(err)
		}
		if card.Document.Avatar.SubjectForm != string(avatardomain.SubjectHuman) || !card.Document.Avatar.PendingUpdate {
			t.Fatalf("card did not retain active subject with pending status: %#v", card.Document.Avatar)
		}
	})
}

func TestSelfCardErrorSanitizesRemoteErrorText(t *testing.T) {
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return selfCardError(errors.New("bad\x1b[2J\u202e\nmessage"))
	})
	body := strings.TrimSuffix(stderr, "\n")
	if code != 1 || stdout != "" || strings.ContainsAny(body, "\x1b\n") || strings.ContainsRune(body, '\u202e') {
		t.Fatalf("selfCardError = %d / %q / %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "bad [2J message") {
		t.Fatalf("sanitized error omitted visible context: %q", stderr)
	}
}

func TestSelfCardConnectionMismatchCannotInjectTerminalControls(t *testing.T) {
	err := verifySelfCardConnection(agentConnection{
		AccountID: "acc_expected", AccountName: "default",
	}, client.SelfIdentity{AccountID: "acc_bad\x1b[2J\u202e"})
	if err == nil {
		t.Fatal("verifySelfCardConnection accepted a mismatched account")
	}
	stdout, stderr, code := captureFactDeleteCLI(t, func() int { return selfCardError(err) })
	body := strings.TrimSuffix(stderr, "\n")
	if code != 1 || stdout != "" || strings.ContainsAny(body, "\x1b\n") || strings.ContainsRune(body, '\u202e') {
		t.Fatalf("account mismatch = %d / %q / %q", code, stdout, stderr)
	}
}

func TestSelfCardTextSanitizationAndBoundedLayout(t *testing.T) {
	cleaned := cleanCardText("Hu\x1b[2J\u202e\n\tgens")
	if strings.ContainsAny(cleaned, "\x1b\n\t") || strings.ContainsRune(cleaned, '\u202e') {
		t.Fatalf("cleanCardText retained terminal or bidi control: %q", cleaned)
	}
	if got := boundedCardText(strings.Repeat("a", 100), 12); ansi.StringWidth(got) > 12 || !strings.HasSuffix(got, "...") {
		t.Fatalf("boundedCardText = %q (width %d)", got, ansi.StringWidth(got))
	}
	if _, err := exactCardIdentifier("agent ID", "agent_1\u202e"); err == nil {
		t.Fatal("exactCardIdentifier accepted a bidi control")
	}

	identity, view := testSelfCardFixture(t, "layout")
	card, err := validateSelfCard(identity, view)
	if err != nil {
		t.Fatal(err)
	}
	var wide bytes.Buffer
	if err := renderPlainSelfCard(&wide, card.Document, selfCardDefaultWidth); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(wide.String(), "+---") || !strings.Contains(wide.String(), "WITSELF AGENT ID") ||
		strings.Contains(wide.String(), "\x1b[") {
		t.Fatalf("wide plain card = %q", wide.String())
	}
	var narrow bytes.Buffer
	if err := renderPlainSelfCard(&narrow, card.Document, 40); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(narrow.String(), "WITSELF AGENT ID\nName: ") || strings.Contains(narrow.String(), "+---") {
		t.Fatalf("narrow card = %q", narrow.String())
	}
}

func TestSelfCardRichRendererUsesFixedANSIHalfBlockPortrait(t *testing.T) {
	identity, view := testSelfCardFixture(t, "render-secret-canary")
	card, err := validateSelfCard(identity, view)
	if err != nil {
		t.Fatal(err)
	}
	portrait, err := rasterizeSelfCardAvatar(card.SVG)
	if err != nil {
		t.Fatal(err)
	}
	if got := portrait.Bounds().Size(); got.X != selfCardRasterSize || got.Y != selfCardRasterSize {
		t.Fatalf("portrait size = %v", got)
	}
	var output bytes.Buffer
	if err := renderRichSelfCard(&output, card.Document, portrait, 72, termenv.TrueColor); err != nil {
		t.Fatal(err)
	}
	rendered := output.String()
	if !strings.Contains(rendered, "\x1b[") || !strings.Contains(rendered, "▀") ||
		!strings.Contains(rendered, "╭") || !strings.Contains(rendered, strings.ToUpper(identity.AgentName)) {
		t.Fatalf("rich card omitted expected presentation: %q", rendered)
	}
	if strings.Contains(rendered, "render-secret-canary") || strings.Contains(rendered, "<svg") {
		t.Fatalf("rich card exposed SVG payload: %q", rendered)
	}
}

func TestSelfCardRasterizesBuiltInDeterministicPlaceholder(t *testing.T) {
	svg, err := avatardomain.GeneratePlaceholderSVG("agent_placeholder", "huygens")
	if err != nil {
		t.Fatal(err)
	}
	portrait, err := rasterizeSelfCardAvatar(svg)
	if err != nil {
		t.Fatalf("rasterize built-in placeholder: %v", err)
	}
	if got := portrait.Bounds().Size(); got.X != selfCardRasterSize || got.Y != selfCardRasterSize {
		t.Fatalf("portrait size = %v", got)
	}
}

func TestSelfCardRichCapabilityGating(t *testing.T) {
	tests := []struct {
		name string
		term selfCardTerminal
		want bool
	}{
		{name: "supported", term: selfCardTerminal{TTY: true, Width: 72, UTF8: true, Profile: termenv.ANSI256}, want: true},
		{name: "pipe", term: selfCardTerminal{TTY: false, Width: 72, UTF8: true, Profile: termenv.TrueColor}},
		{name: "narrow", term: selfCardTerminal{TTY: true, Width: 60, UTF8: true, Profile: termenv.TrueColor}},
		{name: "no color", term: selfCardTerminal{TTY: true, Width: 72, UTF8: true, Profile: termenv.Ascii}},
		{name: "no UTF-8", term: selfCardTerminal{TTY: true, Width: 72, UTF8: false, Profile: termenv.TrueColor}},
		{name: "dumb terminal", term: selfCardTerminal{TTY: true, Width: 72, UTF8: true, Dumb: true, Profile: termenv.TrueColor}},
		{name: "NO_COLOR", term: selfCardTerminal{TTY: true, Width: 72, UTF8: true, NoColor: true, Profile: termenv.TrueColor}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.term.richCapable(); got != tt.want {
				t.Fatalf("richCapable = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestSelfCardRejectsPlainAndJSONTogether(t *testing.T) {
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{"self", "card", "--plain", "--json"})
	})
	if code != 2 || stdout != "" || !strings.Contains(stderr, "usage: witself self card") {
		t.Fatalf("plain+json = %d / %q / %q", code, stdout, stderr)
	}
}

func TestSelfCardForbiddenFieldTripwireCoversPrivateAndUnsettledContent(t *testing.T) {
	for _, key := range []string{
		"svg", "visual_spec", "primary_facts", "salient_memories", "facts", "memories",
		"memory_checkpoint", "message_checkpoint", "avatar_checkpoint", "checkpoint",
		"proposed", "proposal", "proposed_avatar", "proposed_artwork", "proposed_version",
	} {
		if !jsonTreeHasKey(map[string]any{"nested": []any{map[string]any{key: "canary"}}}, selfCardForbiddenFieldSet()) {
			t.Errorf("jsonTreeHasKey did not find %q", key)
		}
	}
}

func testSelfCardFixture(t *testing.T, svgCanary string) (client.SelfIdentity, *client.AvatarView) {
	t.Helper()
	identity := client.SelfIdentity{
		AccountID: "acc_1", AgentID: "agent_1", AgentName: "huygens",
		RealmID: "realm_1", RealmName: "default",
	}
	rawSVG := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" width="512" height="512" role="img" aria-label="Agent portrait"><title>` + svgCanary + `</title><rect x="0" y="0" width="512" height="512" fill="#DCEAF5"></rect><circle cx="256" cy="230" r="112" fill="#E9C46A" stroke="#203247" stroke-width="8"></circle><path d="M128 500 C160 360 352 360 384 500 Z" fill="#2A9D8F"></path></svg>`)
	svg, err := avatardomain.SanitizeSVG(rawSVG)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(svg)
	style := avatardomain.StylePackRef{
		RealmID: identity.RealmID, StylePackID: avatardomain.DefaultStylePackID,
		Version: avatardomain.BuiltInStylePackVersion,
	}
	active := &client.AvatarVersion{
		ID: "avv_1", AccountID: identity.AccountID, RealmID: identity.RealmID, AgentID: identity.AgentID,
		Version: 1, LineageGeneration: 1, SubjectForm: avatardomain.SubjectHuman,
		Description: "A curious scientific agent in a flat circular badge.",
		VisualSpec:  json.RawMessage(`{"expression":"curious"}`), SVG: string(svg),
		SVGSHA256: hex.EncodeToString(sum[:]), Style: style,
		RendererProfile: avatardomain.RendererProfilePerceptualV1,
		ProposedBy:      client.AvatarActor{Kind: "agent", ID: identity.AgentID}, IsActive: true,
		WasActivated: true,
	}
	activatedAt := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	active.LastActivatedAt = &activatedAt
	view := &client.AvatarView{
		Profile: client.AvatarProfile{
			AccountID: identity.AccountID, RealmID: identity.RealmID, AgentID: identity.AgentID,
			SubjectForm: avatardomain.SubjectHuman, AutonomyPolicy: avatardomain.AutonomyAgentSelfManaged,
			Status: avatardomain.StatusActive, Style: style, LineageGeneration: 1,
			ProfileRevision: 3, LatestVersion: 1, ActiveVersion: 1,
		},
		Active: active,
	}
	return identity, view
}
