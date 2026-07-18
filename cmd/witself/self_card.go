package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/fyne-io/oksvg"
	"github.com/muesli/termenv"
	"github.com/srwiley/rasterx"
	avatardomain "github.com/witwave-ai/witself/internal/avatar"
	"github.com/witwave-ai/witself/internal/client"
)

const (
	selfCardSchemaVersion = "witself.self-card.v1"
	selfCardDefaultWidth  = 72
	selfCardMinBoxWidth   = 54
	selfCardMinRichWidth  = 68
	selfCardMaxWidth      = 78
	selfCardRasterSize    = 20
)

type selfCardDocument struct {
	SchemaVersion string           `json:"schema_version"`
	Identity      selfCardIdentity `json:"identity"`
	Avatar        selfCardAvatar   `json:"avatar"`
}

type selfCardIdentity struct {
	AccountID string `json:"account_id"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	RealmID   string `json:"realm_id"`
	RealmName string `json:"realm_name"`
	Role      string `json:"role"`
}

type selfCardAvatar struct {
	Kind            string        `json:"kind"`
	Status          string        `json:"status"`
	Version         int64         `json:"version,omitempty"`
	ProfileRevision int64         `json:"profile_revision"`
	SubjectForm     string        `json:"subject_form"`
	RendererProfile string        `json:"renderer_profile"`
	Description     string        `json:"description"`
	Style           selfCardStyle `json:"style"`
	SVGSHA256       string        `json:"svg_sha256"`
	PendingUpdate   bool          `json:"pending_update,omitempty"`
}

type selfCardStyle struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

type validatedSelfCard struct {
	Document selfCardDocument
	SVG      []byte
}

type selfCardTerminal struct {
	TTY     bool
	Width   int
	UTF8    bool
	Dumb    bool
	NoColor bool
	Profile termenv.Profile
}

func selfCard(args []string) int {
	fs := flag.NewFlagSet("self card", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	realm := fs.String("realm", "", `local realm name (default: WITSELF_REALM or "default")`)
	agent := fs.String("agent", "", "local agent name (default: WITSELF_AGENT)")
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an agent token")
	plain := fs.Bool("plain", false, "disable the color portrait and print a deterministic text card")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || (*plain && *jsonOut) {
		fmt.Fprintln(os.Stderr, "usage: witself self card [--plain | --json] [--account NAME] [--realm NAME] (--agent NAME | --endpoint URL --token-file FILE)")
		return 2
	}

	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		return selfCardError(err)
	}
	digest, err := client.GetSelf(ctx, conn.Endpoint, conn.Token, client.SelfOptions{})
	if err != nil {
		return selfCardError(err)
	}
	if err := verifySelfCardConnection(conn, digest.Identity); err != nil {
		return selfCardError(err)
	}
	view, err := client.GetSelfAvatar(ctx, conn.Endpoint, conn.Token)
	if err != nil {
		return selfCardError(err)
	}
	card, err := validateSelfCard(digest.Identity, view)
	if err != nil {
		return selfCardError(err)
	}
	if *jsonOut {
		if selfCardJSONHasForbiddenFields(card.Document) {
			return selfCardError(errors.New("self card document contains a forbidden content field"))
		}
		return printJSON(card.Document)
	}

	terminal := detectSelfCardTerminal()
	if !*plain && terminal.richCapable() {
		portrait, rasterErr := rasterizeSelfCardAvatar(card.SVG)
		if rasterErr == nil {
			if err := renderRichSelfCard(os.Stdout, card.Document, portrait, terminal.Width, terminal.Profile); err != nil {
				return selfCardError(err)
			}
			return 0
		}
	}
	width := terminal.Width
	if width <= 0 {
		width = selfCardDefaultWidth
	}
	if err := renderPlainSelfCard(os.Stdout, card.Document, width); err != nil {
		return selfCardError(err)
	}
	return 0
}

func selfCardError(err error) int {
	fmt.Fprintf(os.Stderr, "witself: %s\n", boundedCardText(err.Error(), 512))
	return 1
}

func verifySelfCardConnection(conn agentConnection, identity client.SelfIdentity) error {
	if conn.AccountID != "" && identity.AccountID != conn.AccountID {
		return fmt.Errorf("agent token belongs to account %q, not local account %q (%q)",
			identity.AccountID, conn.AccountName, conn.AccountID)
	}
	if conn.RealmName != "" && identity.RealmName != conn.RealmName {
		return fmt.Errorf("agent token belongs to realm %q, not %q", identity.RealmName, conn.RealmName)
	}
	if conn.AgentName != "" && identity.AgentName != conn.AgentName {
		return fmt.Errorf("agent token belongs to agent %q, not %q", identity.AgentName, conn.AgentName)
	}
	return nil
}

func validateSelfCard(identity client.SelfIdentity, view *client.AvatarView) (validatedSelfCard, error) {
	if view == nil || view.Active == nil {
		return validatedSelfCard{}, errors.New("self avatar response has no active avatar or placeholder")
	}
	profile := view.Profile
	active := view.Active
	if profile.AccountID != identity.AccountID || profile.RealmID != identity.RealmID || profile.AgentID != identity.AgentID {
		return validatedSelfCard{}, errors.New("self avatar profile identity does not match the authenticated self identity")
	}
	if active.AccountID != identity.AccountID || active.RealmID != identity.RealmID || active.AgentID != identity.AgentID {
		return validatedSelfCard{}, errors.New("active avatar identity does not match the authenticated self identity")
	}
	if err := profile.Status.Validate(); err != nil {
		return validatedSelfCard{}, fmt.Errorf("validate avatar status: %w", err)
	}
	if err := profile.SubjectForm.Validate(); err != nil {
		return validatedSelfCard{}, fmt.Errorf("validate avatar profile subject: %w", err)
	}
	if err := active.SubjectForm.Validate(); err != nil {
		return validatedSelfCard{}, fmt.Errorf("validate active avatar subject: %w", err)
	}
	// A v186 CLI may briefly read a response from a v185 server that predates
	// explicit renderer provenance. Missing wire metadata is quarantined as
	// legacy and is never promoted or persisted by the client.
	if active.RendererProfile == "" {
		active.RendererProfile = avatardomain.RendererProfileLegacy
	}
	if err := active.RendererProfile.Validate(); err != nil {
		return validatedSelfCard{}, fmt.Errorf("validate active avatar renderer profile: %w", err)
	}
	if err := profile.Style.Validate(); err != nil {
		return validatedSelfCard{}, fmt.Errorf("validate avatar profile style: %w", err)
	}
	if err := active.Style.Validate(); err != nil {
		return validatedSelfCard{}, fmt.Errorf("validate active avatar style: %w", err)
	}
	if profile.Style.RealmID != identity.RealmID || active.Style.RealmID != identity.RealmID {
		return validatedSelfCard{}, errors.New("self avatar style realm does not match the authenticated self identity")
	}
	if active.LineageGeneration != profile.LineageGeneration {
		return validatedSelfCard{}, errors.New("active avatar lineage does not match its profile")
	}
	if profile.ProfileRevision < 1 || profile.LineageGeneration < 1 {
		return validatedSelfCard{}, errors.New("self avatar profile has invalid lifecycle revisions")
	}
	if !active.IsActive || active.IsProposed || active.Rejected {
		return validatedSelfCard{}, errors.New("self avatar response did not identify one safe active presentation")
	}

	kind := "active"
	if profile.ActiveVersion == 0 {
		kind = "placeholder"
		if active.Version != 0 || !strings.HasPrefix(active.ID, "placeholder-") || active.ProposedBy.Kind != "system" ||
			active.SubjectForm != avatardomain.SubjectHuman || active.Style != profile.Style ||
			active.WasActivated || active.LastActivatedAt != nil {
			return validatedSelfCard{}, errors.New("self avatar placeholder metadata is inconsistent")
		}
	} else if active.Version != profile.ActiveVersion || active.Version < 1 {
		return validatedSelfCard{}, errors.New("active avatar version does not match its profile pointer")
	} else if !active.WasActivated || active.LastActivatedAt == nil {
		return validatedSelfCard{}, errors.New("active avatar has no activation evidence")
	}

	if err := avatardomain.ValidateDescription(active.Description); err != nil {
		return validatedSelfCard{}, fmt.Errorf("validate active avatar description: %w", err)
	}
	sanitizedSVG, err := avatardomain.SanitizeSVG([]byte(active.SVG))
	if err != nil {
		return validatedSelfCard{}, fmt.Errorf("validate active avatar SVG: %w", err)
	}
	if !bytes.Equal(sanitizedSVG, []byte(active.SVG)) {
		return validatedSelfCard{}, errors.New("active avatar SVG is not in canonical sanitized form")
	}
	expectedHash, err := hex.DecodeString(active.SVGSHA256)
	if err != nil || len(expectedHash) != sha256.Size {
		return validatedSelfCard{}, errors.New("active avatar SVG hash is not a SHA-256 digest")
	}
	actualHash := sha256.Sum256(sanitizedSVG)
	if subtle.ConstantTimeCompare(expectedHash, actualHash[:]) != 1 {
		return validatedSelfCard{}, errors.New("active avatar SVG hash does not match its sanitized content")
	}

	accountID, err := exactCardIdentifier("account ID", identity.AccountID)
	if err != nil {
		return validatedSelfCard{}, err
	}
	agentID, err := exactCardIdentifier("agent ID", identity.AgentID)
	if err != nil {
		return validatedSelfCard{}, err
	}
	realmID, err := exactCardIdentifier("realm ID", identity.RealmID)
	if err != nil {
		return validatedSelfCard{}, err
	}

	doc := selfCardDocument{
		SchemaVersion: selfCardSchemaVersion,
		Identity: selfCardIdentity{
			AccountID: accountID,
			AgentID:   agentID,
			AgentName: boundedCardText(identity.AgentName, 80),
			RealmID:   realmID,
			RealmName: boundedCardText(identity.RealmName, 80),
			Role:      "AI agent",
		},
		Avatar: selfCardAvatar{
			Kind:            kind,
			Status:          string(profile.Status),
			Version:         active.Version,
			ProfileRevision: profile.ProfileRevision,
			SubjectForm:     string(active.SubjectForm),
			RendererProfile: string(active.RendererProfile),
			Description:     boundedCardText(active.Description, 160),
			Style: selfCardStyle{
				ID:      boundedCardText(active.Style.StylePackID, avatardomain.MaxStylePackIDBytes),
				Version: active.Style.Version,
			},
			SVGSHA256:     hex.EncodeToString(actualHash[:]),
			PendingUpdate: profile.ProposedVersion > 0,
		},
	}
	if doc.Identity.AgentName == "" || doc.Identity.RealmName == "" || doc.Avatar.Description == "" {
		return validatedSelfCard{}, errors.New("self card contains an empty display field after sanitization")
	}
	return validatedSelfCard{Document: doc, SVG: sanitizedSVG}, nil
}

func exactCardIdentifier(label, value string) (string, error) {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) {
		return "", fmt.Errorf("self %s is empty or over the display limit", label)
	}
	cleaned := cleanCardText(value)
	if cleaned != value {
		return "", fmt.Errorf("self %s contains unsafe display characters", label)
	}
	return value, nil
}

func boundedCardText(value string, width int) string {
	cleaned := cleanCardText(value)
	if ansi.StringWidth(cleaned) <= width {
		return cleaned
	}
	return ansi.Truncate(cleaned, width, "...")
}

func cleanCardText(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || isBidiControl(r) {
			return ' '
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

func isBidiControl(r rune) bool {
	return r == '\u061c' || r == '\u200e' || r == '\u200f' ||
		(r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069')
}

func detectSelfCardTerminal() selfCardTerminal {
	fd := os.Stdout.Fd()
	tty := term.IsTerminal(fd)
	width := selfCardDefaultWidth
	if tty {
		if terminalWidth, _, err := term.GetSize(fd); err == nil && terminalWidth > 0 {
			width = terminalWidth
		}
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	return selfCardTerminal{
		TTY: tty, Width: width, UTF8: selfCardUTF8Locale(),
		Dumb:    strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb"),
		NoColor: noColor,
		Profile: termenv.EnvColorProfile(),
	}
}

func (t selfCardTerminal) richCapable() bool {
	return t.TTY && t.Width >= selfCardMinRichWidth && t.UTF8 && !t.Dumb && !t.NoColor && t.Profile != termenv.Ascii
}

func selfCardUTF8Locale() bool {
	locale := os.Getenv("LC_ALL")
	if locale == "" {
		locale = os.Getenv("LC_CTYPE")
	}
	if locale == "" {
		locale = os.Getenv("LANG")
	}
	if locale == "" {
		return true
	}
	locale = strings.ToLower(locale)
	return strings.Contains(locale, "utf-8") || strings.Contains(locale, "utf8")
}

func renderPlainSelfCard(w io.Writer, doc selfCardDocument, terminalWidth int) error {
	if terminalWidth > 0 && terminalWidth < selfCardMinBoxWidth {
		return renderNarrowSelfCard(w, doc)
	}
	width := terminalWidth
	if width <= 0 {
		width = selfCardDefaultWidth
	}
	width = min(width, selfCardMaxWidth)
	width = max(width, selfCardMinBoxWidth)
	inside := width - 2
	lines := []string{
		"WITSELF AGENT ID",
		"",
		strings.ToUpper(doc.Identity.AgentName),
		doc.Identity.Role + " - " + doc.Avatar.SubjectForm,
		"",
		"ID       " + doc.Identity.AgentID,
		"Realm    " + doc.Identity.RealmName + " (" + doc.Identity.RealmID + ")",
		"Account  " + doc.Identity.AccountID,
		"Avatar   " + selfCardAvatarLabel(doc.Avatar),
		"Style    " + selfCardStyleLabel(doc.Avatar.Style),
		"Status   " + selfCardStatusLabel(doc.Avatar),
		"About    " + doc.Avatar.Description,
	}
	if _, err := fmt.Fprintf(w, "+%s+\n", strings.Repeat("-", inside)); err != nil {
		return err
	}
	for _, line := range lines {
		line = truncatePlainCardText(line, inside-2)
		if _, err := fmt.Fprintf(w, "| %s |\n", padCardText(line, inside-2)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "+%s+\n", strings.Repeat("-", inside))
	return err
}

func renderNarrowSelfCard(w io.Writer, doc selfCardDocument) error {
	lines := []string{
		"WITSELF AGENT ID",
		"Name: " + doc.Identity.AgentName,
		"Role: " + doc.Identity.Role + " - " + doc.Avatar.SubjectForm,
		"ID: " + doc.Identity.AgentID,
		"Realm: " + doc.Identity.RealmName + " (" + doc.Identity.RealmID + ")",
		"Account: " + doc.Identity.AccountID,
		"Avatar: " + selfCardAvatarLabel(doc.Avatar),
		"Style: " + selfCardStyleLabel(doc.Avatar.Style),
		"Status: " + selfCardStatusLabel(doc.Avatar),
		"About: " + doc.Avatar.Description,
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func renderRichSelfCard(w io.Writer, doc selfCardDocument, portrait image.Image, terminalWidth int, profile termenv.Profile) error {
	width := min(terminalWidth, selfCardMaxWidth)
	if width < selfCardMinRichWidth {
		return errors.New("terminal is too narrow for the rich self card")
	}
	inside := width - 2
	portraitLines, err := selfCardANSIImage(portrait, profile)
	if err != nil {
		return err
	}
	infoWidth := inside - selfCardRasterSize - 3
	info := []string{
		strings.ToUpper(doc.Identity.AgentName),
		doc.Identity.Role + " - " + doc.Avatar.SubjectForm,
		"",
		"ID       " + doc.Identity.AgentID,
		"Realm    " + doc.Identity.RealmName,
		"Account  " + doc.Identity.AccountID,
		"Avatar   " + selfCardAvatarLabel(doc.Avatar),
		"Style    " + selfCardStyleLabel(doc.Avatar.Style),
		"Status   " + selfCardStatusLabel(doc.Avatar),
		doc.Avatar.Description,
	}
	if len(portraitLines) != len(info) {
		return errors.New("rich self card portrait has an unexpected height")
	}
	if _, err := fmt.Fprintf(w, "╭%s╮\n", strings.Repeat("─", inside)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "│ %s │\n", padCardText("WITSELF AGENT ID", inside-2)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "├%s┤\n", strings.Repeat("─", inside)); err != nil {
		return err
	}
	for i := range portraitLines {
		line := portraitLines[i] + "   " + padCardText(boundedCardText(info[i], infoWidth), infoWidth)
		if _, err := fmt.Fprintf(w, "│%s│\n", padCardText(line, inside)); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "╰%s╯\n", strings.Repeat("─", inside))
	return err
}

func selfCardAvatarLabel(avatar selfCardAvatar) string {
	label := avatar.Kind
	if avatar.Kind == "active" {
		label = fmt.Sprintf("active - version %d", avatar.Version)
	}
	if avatar.PendingUpdate {
		label += " - update pending"
	}
	return label
}

func selfCardStyleLabel(style selfCardStyle) string {
	return fmt.Sprintf("%s - v%d", style.ID, style.Version)
}

func selfCardStatusLabel(avatar selfCardAvatar) string {
	return strings.ReplaceAll(avatar.Status, "_", " ")
}

func truncatePlainCardText(value string, width int) string {
	if ansi.StringWidth(value) <= width {
		return value
	}
	return ansi.Truncate(value, width, "...")
}

func padCardText(value string, width int) string {
	visible := ansi.StringWidth(value)
	if visible >= width {
		return value
	}
	return value + strings.Repeat(" ", width-visible)
}

func rasterizeSelfCardAvatar(svg []byte) (portrait *image.RGBA, err error) {
	canonical, err := avatardomain.SanitizeSVGForPerceptualV1(svg)
	if err != nil {
		return nil, fmt.Errorf("avatar SVG is not compatible with canonical terminal rendering: %w", err)
	}
	defer func() {
		if recover() != nil {
			portrait = nil
			err = errors.New("safe avatar SVG renderer rejected the input")
		}
	}()
	icon, err := oksvg.ReadIconStream(bytes.NewReader(canonical), oksvg.StrictErrorMode)
	if err != nil {
		return nil, fmt.Errorf("parse safe avatar SVG for terminal display: %w", err)
	}
	if icon.ViewBox.W <= 0 || icon.ViewBox.H <= 0 || icon.ViewBox.W > 8192 || icon.ViewBox.H > 8192 ||
		math.Abs(icon.ViewBox.X) > 8192 || math.Abs(icon.ViewBox.Y) > 8192 ||
		math.IsInf(icon.ViewBox.X, 0) || math.IsInf(icon.ViewBox.Y, 0) ||
		math.IsInf(icon.ViewBox.W, 0) || math.IsInf(icon.ViewBox.H, 0) ||
		math.IsNaN(icon.ViewBox.X) || math.IsNaN(icon.ViewBox.Y) ||
		math.IsNaN(icon.ViewBox.W) || math.IsNaN(icon.ViewBox.H) {
		return nil, errors.New("avatar SVG has an unsupported view box")
	}
	portrait = image.NewRGBA(image.Rect(0, 0, selfCardRasterSize, selfCardRasterSize))
	background := color.RGBA{R: 247, G: 250, B: 252, A: 255}
	for y := 0; y < selfCardRasterSize; y++ {
		for x := 0; x < selfCardRasterSize; x++ {
			portrait.SetRGBA(x, y, background)
		}
	}
	scanner := rasterx.NewScannerGV(selfCardRasterSize, selfCardRasterSize, portrait, portrait.Bounds())
	raster := rasterx.NewDasher(selfCardRasterSize, selfCardRasterSize, scanner)
	icon.SetTarget(0, 0, selfCardRasterSize, selfCardRasterSize)
	icon.Draw(raster, 1)
	return portrait, nil
}

func selfCardANSIImage(img image.Image, profile termenv.Profile) ([]string, error) {
	if profile == termenv.Ascii {
		return nil, errors.New("terminal color profile cannot display the avatar")
	}
	bounds := img.Bounds()
	if bounds.Dx() != selfCardRasterSize || bounds.Dy() != selfCardRasterSize {
		return nil, errors.New("avatar raster has an unexpected size")
	}
	lines := make([]string, 0, selfCardRasterSize/2)
	for y := bounds.Min.Y; y < bounds.Max.Y; y += 2 {
		var line strings.Builder
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			foreground := profile.FromColor(img.At(x, y))
			background := profile.FromColor(img.At(x, y+1))
			if foreground == nil || background == nil {
				return nil, errors.New("terminal could not convert an avatar color")
			}
			fmt.Fprintf(&line, "\x1b[%s;%sm▀\x1b[0m", foreground.Sequence(false), background.Sequence(true))
		}
		lines = append(lines, line.String())
	}
	return lines, nil
}

// Ensure accidental future expansion of the presentation document cannot
// silently include the full creative payload. This check is intentionally
// exercised in tests through JSON marshaling as well.
func selfCardJSONHasForbiddenFields(doc selfCardDocument) bool {
	raw, err := json.Marshal(doc)
	if err != nil {
		return true
	}
	var fields any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return true
	}
	return jsonTreeHasKey(fields, selfCardForbiddenFieldSet())
}

func selfCardForbiddenFieldSet() map[string]bool {
	return map[string]bool{
		"svg": true, "visual_spec": true, "primary_facts": true, "salient_memories": true,
		"facts": true, "memories": true, "memory_checkpoint": true, "message_checkpoint": true,
		"avatar_checkpoint": true, "checkpoint": true, "proposed": true, "proposal": true,
		"proposed_avatar": true, "proposed_artwork": true, "proposed_version": true,
	}
}

func jsonTreeHasKey(value any, forbidden map[string]bool) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if forbidden[key] || jsonTreeHasKey(child, forbidden) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if jsonTreeHasKey(child, forbidden) {
				return true
			}
		}
	}
	return false
}
