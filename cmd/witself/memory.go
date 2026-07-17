package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

const memoryCommandUsage = "usage: witself memory capture|show|list|recall|history|adjust|supersede|forget|restore|reactivate|delete|evidence|vector|curate|acceptance ..."

func memoryCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, memoryCommandUsage)
		return 2
	}
	switch args[0] {
	case "capture":
		return memoryCapture(args[1:])
	case "show", "read":
		return memoryShow(args[1:])
	case "list":
		return memoryList(args[1:])
	case "recall":
		return memoryRecall(args[1:])
	case "history":
		return memoryHistory(args[1:])
	case "adjust":
		return memoryAdjust(args[1:])
	case "supersede":
		return memorySupersede(args[1:])
	case "forget", "restore", "reactivate":
		return memoryLifecycle(args[0], args[1:])
	case "delete":
		return memoryDelete(args[1:])
	case "evidence":
		return memoryEvidence(args[1:])
	case "vector", "vectors":
		return memoryVector(args[1:])
	case "curate", "curation":
		return memoryCurate(args[1:])
	case "acceptance":
		return memoryAcceptance(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "witself memory: unknown subcommand %q\n", args[0])
		return 2
	}
}

func memorySupersede(args []string) int {
	memoryID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory supersede", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	expectedVersion := fs.Int64("expected-version", 0, "exact current active source version")
	replacementsFile := fs.String("replacements-file", "", "JSON array of 1-32 replacement capsules ('-' means stdin)")
	reason := fs.String("reason", "", "brief reason for the reversible supersession")
	runtimeName, model, recipe, recipeVersion := memoryClientFlags(fs)
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this one atomic supersession")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if memoryID == "" {
		if fs.NArg() != 1 {
			memorySupersedeUsage()
			return 2
		}
		memoryID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		memorySupersedeUsage()
		return 2
	}
	if memoryID == "" || *expectedVersion < 1 ||
		strings.TrimSpace(*replacementsFile) == "" || strings.TrimSpace(*idempotencyKey) == "" {
		memorySupersedeUsage()
		return 2
	}
	replacements, err := readMemorySupersedeReplacements(*replacementsFile, strings.TrimSpace(*idempotencyKey))
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	result, err := client.SupersedeMemory(ctx, conn.Endpoint, conn.Token, client.SupersedeMemoryInput{
		MemoryID: memoryID, ExpectedVersion: *expectedVersion,
		Replacements: replacements, Reason: strings.TrimSpace(*reason),
		Client:         memoryClientProvenance(*runtimeName, *model, *recipe, *recipeVersion),
		IdempotencyKey: strings.TrimSpace(*idempotencyKey),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: supersede memory: %v\n", err)
		return 1
	}
	if result.Receipt.ReplacementCount != int64(len(result.Receipt.Replacements)) ||
		result.Receipt.ReplacementCount != int64(len(result.Replacements)) ||
		!factCandidateRevisionPattern.MatchString(result.Receipt.ReplacementDigest) {
		fmt.Fprintln(os.Stderr, "witself: supersede memory: server returned an incomplete or inconsistent supersession receipt")
		return 1
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("superseded\tsource=%s@%d\tset=%s@%d\treplacements=%d\treplacement-digest=%s\treplayed=%t\n",
		result.Receipt.Source.MemoryID, result.Receipt.Source.Version,
		result.Receipt.SupersessionSetID, result.Receipt.SupersessionSetRevision,
		result.Receipt.ReplacementCount, result.Receipt.ReplacementDigest, result.Receipt.Replayed)
	for _, replacement := range result.Receipt.Replacements {
		fmt.Printf("replacement\tmemory=%s\tversion=%d\n", replacement.MemoryID, replacement.Version)
	}
	return 0
}

func memorySupersedeUsage() {
	fmt.Fprintln(os.Stderr, "usage: witself memory supersede MEM_ID --expected-version N --replacements-file FILE --idempotency-key KEY [flags]")
}

func readMemorySupersedeReplacements(path, operationKey string) ([]client.SupersedeMemoryReplacementInput, error) {
	raw, err := readBodyFromFlags("", strings.TrimSpace(path), false)
	if err != nil {
		return nil, fmt.Errorf("read --replacements-file: %w", err)
	}
	var replacements []client.SupersedeMemoryReplacementInput
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&replacements); err != nil {
		return nil, fmt.Errorf("decode --replacements-file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, fmt.Errorf("decode --replacements-file: %w", err)
	}
	if len(replacements) < 1 || len(replacements) > 32 {
		return nil, fmt.Errorf("--replacements-file must contain 1-32 replacement capsules")
	}
	seenKeys := map[string]struct{}{operationKey: {}}
	for i := range replacements {
		replacement := &replacements[i]
		replacement.IdempotencyKey = strings.TrimSpace(replacement.IdempotencyKey)
		if strings.TrimSpace(replacement.Content) == "" || replacement.IdempotencyKey == "" || len(replacement.Evidence) == 0 {
			return nil, fmt.Errorf("replacement %d requires content, evidence, and idempotency_key", i)
		}
		replacement.ContentEncoding, err = normalizeMemoryContentEncoding(replacement.ContentEncoding)
		if err != nil {
			return nil, fmt.Errorf("replacement %d: %w", i, err)
		}
		if _, exists := seenKeys[replacement.IdempotencyKey]; exists {
			return nil, fmt.Errorf("replacement %d reuses an operation or replacement idempotency_key", i)
		}
		seenKeys[replacement.IdempotencyKey] = struct{}{}
	}
	return replacements, nil
}

func memoryDelete(args []string) int {
	memoryID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	dryRun := fs.Bool("dry-run", false, "return a value-free deletion preview without mutating memory")
	yes := fs.Bool("yes", false, "confirm irreversible permanent deletion on a direct current-user request")
	expectedVersion := fs.Int64("expected-version", 0, "exact current version returned by preview")
	scrubSetRevision := fs.String("scrub-set-revision", "", "exact lowercase SHA-256 scrub revision returned by preview")
	idempotencyKey := fs.String("idempotency-key", "", "fresh retry key for this one logical permanent deletion")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if memoryID == "" {
		if fs.NArg() != 1 {
			memoryDeleteUsage()
			return 2
		}
		memoryID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		memoryDeleteUsage()
		return 2
	}
	if memoryID == "" || (*dryRun && *yes) {
		memoryDeleteUsage()
		return 2
	}
	if *dryRun {
		if *expectedVersion != 0 || strings.TrimSpace(*scrubSetRevision) != "" || strings.TrimSpace(*idempotencyKey) != "" {
			fmt.Fprintln(os.Stderr, "witself: --dry-run accepts only the memory id and connection/output flags")
			return 2
		}
	} else {
		if !*yes {
			fmt.Fprintln(os.Stderr, "witself: memory deletion is permanent and cannot be undone; preview with --dry-run, then apply the exact guards with --yes")
			return 2
		}
		if *expectedVersion < 1 || !factCandidateRevisionPattern.MatchString(strings.TrimSpace(*scrubSetRevision)) || strings.TrimSpace(*idempotencyKey) == "" {
			fmt.Fprintln(os.Stderr, "witself: apply requires --expected-version, a 64-character lowercase --scrub-set-revision, --idempotency-key, and --yes")
			return 2
		}
	}

	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *dryRun {
		receipt, err := client.PreviewDeleteMemory(ctx, conn.Endpoint, conn.Token, memoryID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: preview permanent memory deletion: %v\n", err)
			return 1
		}
		if err := validateMemoryDeletionPreview(*receipt, memoryID); err != nil {
			fmt.Fprintf(os.Stderr, "witself: preview permanent memory deletion: %v\n", err)
			return 1
		}
		return printMemoryDeletionReceipt(*receipt, true, *jsonOut)
	}

	receipt, err := client.DeleteMemory(ctx, conn.Endpoint, conn.Token, client.DeleteMemoryInput{
		MemoryID: memoryID, ExpectedVersion: *expectedVersion,
		ScrubSetRevision: strings.TrimSpace(*scrubSetRevision),
		IdempotencyKey:   strings.TrimSpace(*idempotencyKey), DirectUserAuthorized: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: apply or replay permanent memory deletion: %v\n", err)
		return 1
	}
	if err := validateAppliedMemoryDeletion(*receipt, memoryID, *expectedVersion, strings.TrimSpace(*scrubSetRevision)); err != nil {
		fmt.Fprintf(os.Stderr, "witself: apply or replay permanent memory deletion: %v\n", err)
		return 1
	}
	return printMemoryDeletionReceipt(*receipt, false, *jsonOut)
}

func memoryDeleteUsage() {
	fmt.Fprintln(os.Stderr, "usage: witself memory delete MEM_ID --dry-run")
	fmt.Fprintln(os.Stderr, "       witself memory delete MEM_ID --yes --expected-version N --scrub-set-revision SHA256 --idempotency-key KEY")
}

func validateMemoryDeletionPreview(receipt client.MemoryDeletionReceipt, memoryID string) error {
	if receipt.Applied || receipt.ReceiptID != "" || receipt.DeletedAt != nil {
		return fmt.Errorf("server marked a deletion preview as applied")
	}
	if receipt.MemoryID != memoryID || receipt.PriorVersion < 1 ||
		!factCandidateRevisionPattern.MatchString(receipt.ScrubSetRevision) ||
		!factCandidateRevisionPattern.MatchString(receipt.RetryShieldDigest) ||
		receipt.VersionCount < 1 || receipt.EvidenceCount < 1 || receipt.RetryShieldCount < receipt.VersionCount {
		return fmt.Errorf("server returned an incomplete or inconsistent deletion preview")
	}
	return nil
}

func validateAppliedMemoryDeletion(receipt client.MemoryDeletionReceipt, memoryID string, expectedVersion int64, scrubSetRevision string) error {
	if !receipt.Applied || receipt.ReceiptID == "" || receipt.DeletedAt == nil ||
		receipt.MemoryID != memoryID || receipt.PriorVersion != expectedVersion ||
		receipt.ScrubSetRevision != scrubSetRevision || receipt.Blocked {
		return fmt.Errorf("server returned an incomplete or inconsistent deletion receipt")
	}
	return nil
}

func printMemoryDeletionReceipt(receipt client.MemoryDeletionReceipt, dryRun, jsonOut bool) int {
	if jsonOut {
		return printJSON(map[string]any{"deletion": receipt})
	}
	action := "would permanently delete"
	if !dryRun {
		action = "permanently deleted"
	}
	fmt.Printf("%s\tmemory=%s\treceipt=%s\tversion=%d\tscrub-set-revision=%s\tversions=%d\tevidence=%d\trelations=%d\tretry-shields=%d\tincoming-evidence=%d\tactive-dependencies=%d\tactive-curation-dependencies=%d\tcuration-runs=%d\tcuration-actions=%d\tcuration-inputs=%d\tcuration-mutations=%d\tblocked=%t\treplayed=%t\n",
		action, receipt.MemoryID, receipt.ReceiptID, receipt.PriorVersion,
		receipt.ScrubSetRevision, receipt.VersionCount, receipt.EvidenceCount,
		receipt.RelationCount, receipt.RetryShieldCount, receipt.IncomingEvidenceCount,
		receipt.ActiveRelationDependencyCount, receipt.ActiveCurationDependencyCount,
		receipt.CurationRunCount, receipt.CurationActionCount,
		receipt.CurationInputCount, receipt.CurationMutationCount,
		receipt.Blocked, receipt.Replayed)
	return 0
}

func memoryRecall(args []string) int {
	query, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory recall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	queryFlag := fs.String("query", "", "literal full-text query; natural date interpretation is client-side")
	kind := fs.String("kind", "", "exact memory kind filter")
	var tags, links csvListFlag
	fs.Var(&tags, "tag", "required tag (repeatable or comma-separated)")
	fs.Var(&links, "link", "required identity link (repeatable or comma-separated)")
	origin := fs.String("origin", "", "immutable origin filter")
	captureReason := fs.String("capture-reason", "", "capture trigger filter")
	includeSensitive := fs.Bool("include-sensitive", false, "include sensitive content when intentionally required")
	occurredFromRaw := fs.String("occurred-from", "", "event range lower bound in RFC3339")
	occurredUntilRaw := fs.String("occurred-until", "", "event range upper bound in RFC3339")
	capturedFromRaw := fs.String("captured-from", "", "capture range lower bound in RFC3339")
	capturedUntilRaw := fs.String("captured-until", "", "capture range upper bound in RFC3339")
	limit := fs.Int("limit", 20, "maximum ranked hits from 1 to 100")
	cursor := fs.String("cursor", "", "opaque continuation cursor")
	vectorProfile := fs.String("vector-profile", "", "immutable client vector profile id")
	queryVectorFile := fs.String("query-vector-file", "", "JSON array of finite query-vector numbers ('-' means stdin)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		if query != "" || *queryFlag != "" || fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: witself memory recall [QUERY] [filters]")
			return 2
		}
		query = strings.TrimSpace(fs.Arg(0))
	}
	if *queryFlag != "" {
		if query != "" {
			fmt.Fprintln(os.Stderr, "witself: positional QUERY and --query are mutually exclusive")
			return 2
		}
		query = strings.TrimSpace(*queryFlag)
	}
	if *limit < 1 || *limit > 100 {
		fmt.Fprintln(os.Stderr, "witself: --limit must be between 1 and 100")
		return 2
	}
	occurredFrom, occurredUntil, err := memoryTimeRange(*occurredFromRaw, *occurredUntilRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	capturedFrom, capturedUntil, err := memoryTimeRange(*capturedFromRaw, *capturedUntilRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	queryVector, err := readMemoryVectorFile(*queryVectorFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if (strings.TrimSpace(*vectorProfile) == "") != (len(queryVector) == 0) {
		fmt.Fprintln(os.Stderr, "witself: --vector-profile and --query-vector-file must be supplied together")
		return 2
	}
	if query == "" && strings.TrimSpace(*kind) == "" && len(tags) == 0 && len(links) == 0 &&
		strings.TrimSpace(*origin) == "" && strings.TrimSpace(*captureReason) == "" &&
		occurredFrom == nil && occurredUntil == nil && capturedFrom == nil && capturedUntil == nil && len(queryVector) == 0 {
		fmt.Fprintln(os.Stderr, "witself: recall requires QUERY or at least one structured filter")
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	page, err := client.RecallMemories(ctx, conn.Endpoint, conn.Token, client.MemoryRecallInput{
		Query: query, Kind: *kind, Tags: tags, Links: links,
		Origin: *origin, CaptureReason: *captureReason,
		IncludeSensitive: *includeSensitive,
		OccurredFrom:     occurredFrom, OccurredUntil: occurredUntil,
		CapturedFrom: capturedFrom, CapturedUntil: capturedUntil,
		Limit: *limit, Cursor: *cursor,
		VectorProfileID: strings.TrimSpace(*vectorProfile), QueryVector: queryVector,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if page.Hits == nil {
		page.Hits = []client.MemoryRecallHit{}
	}
	if *jsonOut {
		return printJSON(page)
	}
	if len(page.Hits) == 0 {
		fmt.Fprintln(os.Stderr, "no matching memories")
		return 0
	}
	w, flush := tableWriter("score\tsimilarity\tvector\tlexical\tsalience\trecency\tupdated\tid\tkind\tencoding\tcontent")
	for _, hit := range page.Hits {
		_, _ = fmt.Fprintf(w, "%.4f\t%.4f\t%t\t%.4f\t%.4f\t%.4f\t%s\t%s\t%s\t%s\t%s\n",
			hit.Score.Total, hit.Score.Similarity, hit.Score.VectorUsed,
			hit.Score.Lexical, hit.Score.Salience, hit.Score.Recency,
			formatTime(hit.Memory.UpdatedAt), hit.Memory.ID, tabSafe(hit.Memory.Kind),
			tabSafe(hit.Memory.ContentEncoding),
			tabSafe(memoryPreview(hit.Memory.Content, hit.Memory.Redacted)))
	}
	flush()
	if page.NextCursor != "" {
		fmt.Fprintf(os.Stderr, "more memories available; continue with --cursor %s\n", page.NextCursor)
	}
	return 0
}

func memoryEvidence(args []string) int {
	if len(args) == 0 || args[0] != "resolve" {
		fmt.Fprintln(os.Stderr, "usage: witself memory evidence resolve EVIDENCE_ID --idempotency-key KEY [source flags]")
		return 2
	}
	evidenceID, args := memoryLeadingID(args[1:])
	fs := flag.NewFlagSet("memory evidence resolve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	transcriptID := fs.String("transcript", "", "exact Witself transcript id")
	fromSequence := fs.Int64("from-sequence", 0, "first exact transcript entry sequence")
	untilSequence := fs.Int64("until-sequence", 0, "last exact transcript entry sequence")
	sourceMemoryID := fs.String("memory", "", "exact source memory id")
	sourceMemoryVersion := fs.Int64("memory-version", 0, "exact immutable source memory version")
	messageID := fs.String("message", "", "exact realm message id")
	importArtifactID := fs.String("import-artifact", "", "stable imported artifact locator")
	unresolvableReason := fs.String("unresolvable-reason", "", "bounded reason code when the pending locator cannot resolve")
	sourceDigest := fs.String("source-digest", "", "optional lowercase SHA-256 source digest")
	idempotencyKey := fs.String("idempotency-key", "", "retry key for exactly one evidence resolution")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if evidenceID == "" {
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: witself memory evidence resolve EVIDENCE_ID --idempotency-key KEY [source flags]")
			return 2
		}
		evidenceID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory evidence resolve EVIDENCE_ID --idempotency-key KEY [source flags]")
		return 2
	}
	if evidenceID == "" || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself memory evidence resolve EVIDENCE_ID --idempotency-key KEY [source flags]")
		return 2
	}
	transcriptMode := strings.TrimSpace(*transcriptID) != "" || *fromSequence != 0 || *untilSequence != 0
	memoryMode := strings.TrimSpace(*sourceMemoryID) != "" || *sourceMemoryVersion != 0
	messageMode := strings.TrimSpace(*messageID) != ""
	importMode := strings.TrimSpace(*importArtifactID) != ""
	unresolvableMode := strings.TrimSpace(*unresolvableReason) != ""
	if boolCount(transcriptMode, memoryMode, messageMode, importMode, unresolvableMode) != 1 {
		fmt.Fprintln(os.Stderr, "witself: choose exactly one transcript, memory, message, import-artifact, or unresolvable source")
		return 2
	}
	if transcriptMode && (strings.TrimSpace(*transcriptID) == "" || *fromSequence < 1 || *untilSequence < *fromSequence) {
		fmt.Fprintln(os.Stderr, "witself: transcript resolution requires --transcript and a positive ordered sequence range")
		return 2
	}
	if memoryMode && (strings.TrimSpace(*sourceMemoryID) == "" || *sourceMemoryVersion < 1) {
		fmt.Fprintln(os.Stderr, "witself: memory resolution requires --memory and a positive --memory-version")
		return 2
	}
	in := client.ResolveMemoryEvidenceInput{
		EvidenceID: evidenceID, TranscriptID: *transcriptID,
		SourceMemoryID: *sourceMemoryID, MessageID: *messageID,
		ImportArtifactID: *importArtifactID, UnresolvableReason: *unresolvableReason,
		SourceDigest: *sourceDigest, IdempotencyKey: *idempotencyKey,
	}
	if transcriptMode {
		from, until := *fromSequence, *untilSequence
		in.EntryFromSequence, in.EntryUntilSequence = &from, &until
	}
	if memoryMode {
		version := *sourceMemoryVersion
		in.SourceMemoryVersion = &version
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	evidence, err := client.ResolveMemoryEvidence(ctx, conn.Endpoint, conn.Token, in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"evidence": evidence})
	}
	fmt.Printf("%s\t%s\t%s\t%d\n", evidence.ID, evidence.State, evidence.MemoryID, evidence.MemoryVersion)
	return 0
}

func memoryCapture(args []string) int {
	fs := flag.NewFlagSet("memory capture", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	content := fs.String("content", "", "client-authored narrative content")
	contentFile := fs.String("file", "", "read narrative content from FILE ('-' means stdin)")
	stdin := fs.Bool("stdin", false, "read narrative content from stdin")
	contentEncodingRaw := fs.String("content-encoding", "plain", "content encoding: plain or canonical base64")
	kind := fs.String("kind", "note", "memory kind such as decision, session, milestone, or lesson")
	var tags, links csvListFlag
	fs.Var(&tags, "tag", "memory tag (repeatable or comma-separated)")
	fs.Var(&links, "link", "typed identity link (repeatable or comma-separated)")
	salienceRaw := fs.Float64("salience", -1, "salience from 0 to 1 (omit for the service default)")
	sensitive := fs.Bool("sensitive", false, "redact content in broad list and recall results")
	occurredFromRaw := fs.String("occurred-from", "", "event range start in RFC3339")
	occurredUntilRaw := fs.String("occurred-until", "", "event range end in RFC3339")
	captureReason := fs.String("capture-reason", "manual", "capture trigger such as explicit, automatic, session, or manual")
	evidenceFile := fs.String("evidence-file", "", "JSON array of typed evidence objects")
	evidenceTranscript := fs.String("evidence-transcript", "", "exact Witself transcript id")
	evidenceFrom := fs.Int64("evidence-from-sequence", 0, "first exact transcript entry sequence")
	evidenceUntil := fs.Int64("evidence-until-sequence", 0, "last exact transcript entry sequence")
	evidenceLocator := fs.String("evidence-locator", "", "stable external locator for evidence not flushed yet")
	evidenceUnavailable := fs.String("evidence-unavailable-reason", "", "why exact or pending evidence is unavailable")
	evidenceRole := fs.String("evidence-role", "supports", "evidence role: supports, contradicts, or context")
	evidenceDigest := fs.String("evidence-source-digest", "", "optional digest supplied by the client")
	runtimeName, model, recipe, recipeVersion := memoryClientFlags(fs)
	idempotencyKey := fs.String("idempotency-key", "", "retry key for exactly one logical capture")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory capture [flags] (--content TEXT|--file FILE|--stdin)")
		return 2
	}
	text, err := readBodyFromFlags(*content, *contentFile, *stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(os.Stderr, "witself: --content, --file, or --stdin is required and must not be empty")
		return 2
	}
	if strings.TrimSpace(*kind) == "" || strings.TrimSpace(*captureReason) == "" {
		fmt.Fprintln(os.Stderr, "witself: --kind and --capture-reason must not be empty")
		return 2
	}
	if strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "witself: --idempotency-key is required for memory capture")
		return 2
	}
	contentEncoding, err := normalizeMemoryContentEncoding(*contentEncodingRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: --content-encoding %v\n", err)
		return 2
	}
	salience, err := memoryOptionalUnitFloat(*salienceRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: --salience %v\n", err)
		return 2
	}
	occurredFrom, occurredUntil, err := memoryTimeRange(*occurredFromRaw, *occurredUntilRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	evidence, err := memoryCaptureEvidence(memoryEvidenceFlags{
		File: *evidenceFile, TranscriptID: *evidenceTranscript,
		FromSequence: *evidenceFrom, UntilSequence: *evidenceUntil,
		ExternalLocator: *evidenceLocator, UnavailableReason: *evidenceUnavailable,
		Role: *evidenceRole, SourceDigest: *evidenceDigest,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: evidence: %v\n", err)
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	result, err := client.CaptureMemory(ctx, conn.Endpoint, conn.Token, client.CaptureMemoryInput{
		Content: text, ContentEncoding: contentEncoding,
		Kind: *kind, Tags: tags, Salience: salience,
		Sensitive: *sensitive, Links: links,
		OccurredFrom: occurredFrom, OccurredUntil: occurredUntil,
		Evidence: evidence, CaptureReason: *captureReason,
		Client:         memoryClientProvenance(*runtimeName, *model, *recipe, *recipeVersion),
		IdempotencyKey: *idempotencyKey,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	return printMemoryMutation(result, *jsonOut)
}

func memoryShow(args []string) int {
	memoryID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory show", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if memoryID == "" {
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: witself memory show MEM_ID [flags]")
			return 2
		}
		memoryID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory show MEM_ID [flags]")
		return 2
	}
	if memoryID == "" {
		fmt.Fprintln(os.Stderr, "usage: witself memory show MEM_ID [flags]")
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	memory, err := client.GetMemory(ctx, conn.Endpoint, conn.Token, memoryID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"memory": memory})
	}
	printMemory(memory)
	return 0
}

func memoryList(args []string) int {
	fs := flag.NewFlagSet("memory list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	ownerAgentID := fs.String("owner-agent", "", "filter by stable owner agent id")
	state := fs.String("state", "", "filter by lifecycle state")
	kind := fs.String("kind", "", "filter by memory kind")
	var tags csvListFlag
	fs.Var(&tags, "tag", "filter by tag (repeatable or comma-separated)")
	origin := fs.String("origin", "", "filter by immutable origin")
	captureReason := fs.String("capture-reason", "", "filter by capture reason")
	includeSensitive := fs.Bool("include-sensitive", false, "include sensitive content when authorized")
	occurredFromRaw := fs.String("occurred-from", "", "filter events beginning at or after RFC3339 time")
	occurredUntilRaw := fs.String("occurred-until", "", "filter events ending at or before RFC3339 time")
	capturedFromRaw := fs.String("captured-from", "", "filter captures at or after RFC3339 time")
	capturedUntilRaw := fs.String("captured-until", "", "filter captures at or before RFC3339 time")
	limit := fs.Int("limit", 100, "maximum memories to return, from 1 to 100")
	cursor := fs.String("cursor", "", "opaque continuation cursor")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory list [flags]")
		return 2
	}
	if *limit < 1 || *limit > 100 {
		fmt.Fprintln(os.Stderr, "witself: --limit must be between 1 and 100")
		return 2
	}
	occurredFrom, occurredUntil, err := memoryTimeRange(*occurredFromRaw, *occurredUntilRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	capturedFrom, capturedUntil, err := memoryTimeRange(*capturedFromRaw, *capturedUntilRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	page, err := client.ListMemories(ctx, conn.Endpoint, conn.Token, client.MemoryListOptions{
		OwnerAgentID: *ownerAgentID, State: *state, Kind: *kind, Tags: tags,
		Origin: *origin, CaptureReason: *captureReason,
		IncludeSensitive: *includeSensitive,
		OccurredFrom:     occurredFrom, OccurredUntil: occurredUntil,
		CapturedFrom: capturedFrom, CapturedUntil: capturedUntil,
		Limit: *limit, Cursor: *cursor,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if page.Items == nil {
		page.Items = []client.Memory{}
	}
	if *jsonOut {
		return printJSON(page)
	}
	if len(page.Items) == 0 {
		fmt.Fprintln(os.Stderr, "no memories")
		return 0
	}
	w, flush := tableWriter("updated\tstate\tversion\tid\tkind\tencoding\tsalience\tsensitive\tcontent")
	for _, memory := range page.Items {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%.2f\t%t\t%s\n",
			formatTime(memory.UpdatedAt), memory.State, memory.Version, memory.ID,
			tabSafe(memory.Kind), tabSafe(memory.ContentEncoding), memory.Salience, memory.Sensitive,
			tabSafe(memoryPreview(memory.Content, memory.Redacted)))
	}
	flush()
	if page.NextCursor != "" {
		fmt.Fprintf(os.Stderr, "more memories available; continue with --cursor %s\n", page.NextCursor)
	}
	return 0
}

func memoryHistory(args []string) int {
	memoryID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory history", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	limit := fs.Int("limit", 100, "maximum versions to return, from 1 to 100")
	cursor := fs.String("cursor", "", "opaque continuation cursor")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if memoryID == "" {
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: witself memory history MEM_ID [--limit 1..100] [--cursor CURSOR]")
			return 2
		}
		memoryID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory history MEM_ID [--limit 1..100] [--cursor CURSOR]")
		return 2
	}
	if memoryID == "" || *limit < 1 || *limit > 100 {
		fmt.Fprintln(os.Stderr, "usage: witself memory history MEM_ID [--limit 1..100] [--cursor CURSOR]")
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	page, err := client.GetMemoryHistory(ctx, conn.Endpoint, conn.Token, memoryID, client.MemoryHistoryOptions{
		Limit: *limit, Cursor: *cursor,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	if page.Versions == nil {
		page.Versions = []client.MemoryVersion{}
	}
	if *jsonOut {
		return printJSON(page)
	}
	w, flush := tableWriter("created\tstate\tversion\toperation\tkind\tencoding\treceipt-set\treceipt-revision\treplacement-count\treplacement-digest\tactive-set\tactive-revision\tsensitive\tcontent")
	for _, version := range page.Versions {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%d\t%t\t%s\n",
			formatTime(version.CreatedAt), version.State, version.Version,
			version.Operation, tabSafe(version.Kind), tabSafe(version.ContentEncoding),
			tabSafe(version.SupersessionSetID), version.SupersessionSetRevision,
			version.SupersessionReplacementCount, tabSafe(version.SupersessionReplacementDigest),
			tabSafe(version.ActiveSupersessionSetID), version.ActiveSupersessionSetRevision,
			version.Sensitive,
			tabSafe(memoryPreview(version.Content, version.Redacted)))
	}
	flush()
	if page.NextCursor != "" {
		fmt.Fprintf(os.Stderr, "more versions available; continue with --cursor %s\n", page.NextCursor)
	}
	return 0
}

func memoryAdjust(args []string) int {
	memoryID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory adjust", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	expectedVersion := fs.Int64("expected-version", 0, "required current version concurrency guard")
	content := fs.String("content", "", "replace narrative content")
	contentFile := fs.String("file", "", "read replacement content from FILE ('-' means stdin)")
	stdin := fs.Bool("stdin", false, "read replacement content from stdin")
	contentEncodingRaw := fs.String("content-encoding", "", "replace content encoding: plain or canonical base64")
	kind := fs.String("kind", "", "replace memory kind")
	var addTags, removeTags, addLinks, removeLinks csvListFlag
	fs.Var(&addTags, "add-tag", "add tag (repeatable or comma-separated)")
	fs.Var(&removeTags, "remove-tag", "remove tag (repeatable or comma-separated)")
	fs.Var(&addLinks, "add-link", "add typed link (repeatable or comma-separated)")
	fs.Var(&removeLinks, "remove-link", "remove typed link (repeatable or comma-separated)")
	salienceRaw := fs.Float64("salience", -1, "replace salience with a value from 0 to 1")
	sensitive := fs.Bool("sensitive", false, "mark content sensitive")
	notSensitive := fs.Bool("not-sensitive", false, "clear the sensitive marker")
	occurredFromRaw := fs.String("occurred-from", "", "replace event range start with RFC3339 time")
	occurredUntilRaw := fs.String("occurred-until", "", "replace event range end with RFC3339 time")
	clearOccurredFrom := fs.Bool("clear-occurred-from", false, "clear event range start")
	clearOccurredUntil := fs.Bool("clear-occurred-until", false, "clear event range end")
	reason := fs.String("reason", "", "brief reason for the adjustment")
	runtimeName, model, recipe, recipeVersion := memoryClientFlags(fs)
	idempotencyKey := fs.String("idempotency-key", "", "retry key for exactly one logical adjustment")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if memoryID == "" {
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: witself memory adjust MEM_ID --expected-version N --idempotency-key KEY [patch flags]")
			return 2
		}
		memoryID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory adjust MEM_ID --expected-version N --idempotency-key KEY [patch flags]")
		return 2
	}
	if memoryID == "" || *expectedVersion < 1 || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintln(os.Stderr, "usage: witself memory adjust MEM_ID --expected-version N --idempotency-key KEY [patch flags]")
		return 2
	}
	if *sensitive && *notSensitive {
		fmt.Fprintln(os.Stderr, "witself: --sensitive and --not-sensitive are mutually exclusive")
		return 2
	}
	if *clearOccurredFrom && strings.TrimSpace(*occurredFromRaw) != "" {
		fmt.Fprintln(os.Stderr, "witself: --occurred-from and --clear-occurred-from are mutually exclusive")
		return 2
	}
	if *clearOccurredUntil && strings.TrimSpace(*occurredUntilRaw) != "" {
		fmt.Fprintln(os.Stderr, "witself: --occurred-until and --clear-occurred-until are mutually exclusive")
		return 2
	}
	setContent, err := memoryOptionalContent(*content, *contentFile, *stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	var setContentEncoding *string
	if flagWasPassed(fs, "content-encoding") {
		value, err := normalizeMemoryContentEncoding(*contentEncodingRaw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "witself: --content-encoding %v\n", err)
			return 2
		}
		setContentEncoding = &value
	}
	setSalience, err := memoryOptionalUnitFloat(*salienceRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: --salience %v\n", err)
		return 2
	}
	setOccurredFrom, setOccurredUntil, err := memoryTimeRange(*occurredFromRaw, *occurredUntilRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	var setKind *string
	if flagWasPassed(fs, "kind") {
		if strings.TrimSpace(*kind) == "" {
			fmt.Fprintln(os.Stderr, "witself: --kind must not be empty")
			return 2
		}
		setKind = kind
	}
	var setSensitive *bool
	if *sensitive || *notSensitive {
		value := *sensitive
		setSensitive = &value
	}
	if setContent == nil && setContentEncoding == nil && setKind == nil && len(addTags) == 0 && len(removeTags) == 0 &&
		setSalience == nil && len(addLinks) == 0 && len(removeLinks) == 0 &&
		setSensitive == nil && setOccurredFrom == nil && setOccurredUntil == nil &&
		!*clearOccurredFrom && !*clearOccurredUntil {
		fmt.Fprintln(os.Stderr, "witself: at least one memory patch flag is required")
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	result, err := client.AdjustMemory(ctx, conn.Endpoint, conn.Token, client.AdjustMemoryInput{
		MemoryID: memoryID, ExpectedVersion: *expectedVersion,
		SetContent: setContent, SetContentEncoding: setContentEncoding, SetKind: setKind,
		AddTags: addTags, RemoveTags: removeTags,
		SetSalience: setSalience, AddLinks: addLinks, RemoveLinks: removeLinks,
		SetSensitive:    setSensitive,
		SetOccurredFrom: setOccurredFrom, ClearOccurredFrom: *clearOccurredFrom,
		SetOccurredUntil: setOccurredUntil, ClearOccurredUntil: *clearOccurredUntil,
		Reason:         *reason,
		Client:         memoryClientProvenance(*runtimeName, *model, *recipe, *recipeVersion),
		IdempotencyKey: *idempotencyKey,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	return printMemoryMutation(result, *jsonOut)
}

func memoryLifecycle(action string, args []string) int {
	memoryID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory "+action, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	expectedVersion := fs.Int64("expected-version", 0, "required current version concurrency guard")
	expectedSetRevision := fs.Int64("expected-supersession-set-revision", 0, "reactivate guard for a superseded memory")
	reason := fs.String("reason", "", "brief lifecycle reason")
	runtimeName, model, recipe, recipeVersion := memoryClientFlags(fs)
	idempotencyKey := fs.String("idempotency-key", "", "retry key for exactly one logical lifecycle change")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if memoryID == "" {
		if fs.NArg() != 1 {
			fmt.Fprintf(os.Stderr, "usage: witself memory %s MEM_ID --expected-version N --idempotency-key KEY\n", action)
			return 2
		}
		memoryID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "usage: witself memory %s MEM_ID --expected-version N --idempotency-key KEY\n", action)
		return 2
	}
	if memoryID == "" || *expectedVersion < 1 || strings.TrimSpace(*idempotencyKey) == "" {
		fmt.Fprintf(os.Stderr, "usage: witself memory %s MEM_ID --expected-version N --idempotency-key KEY\n", action)
		return 2
	}
	setRevisionPassed := flagWasPassed(fs, "expected-supersession-set-revision")
	if action != "reactivate" && setRevisionPassed {
		fmt.Fprintln(os.Stderr, "witself: --expected-supersession-set-revision is valid only for reactivate")
		return 2
	}
	if setRevisionPassed && *expectedSetRevision < 1 {
		fmt.Fprintln(os.Stderr, "witself: --expected-supersession-set-revision must be positive")
		return 2
	}
	var setRevision *int64
	if setRevisionPassed {
		setRevision = expectedSetRevision
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	in := client.MemoryLifecycleInput{
		MemoryID: memoryID, ExpectedVersion: *expectedVersion,
		ExpectedSupersessionSetRevision: setRevision, Reason: *reason,
		Client:         memoryClientProvenance(*runtimeName, *model, *recipe, *recipeVersion),
		IdempotencyKey: *idempotencyKey,
	}
	var result *client.MemoryMutationResult
	switch action {
	case "forget":
		result, err = client.ForgetMemory(ctx, conn.Endpoint, conn.Token, in)
	case "restore":
		result, err = client.RestoreMemory(ctx, conn.Endpoint, conn.Token, in)
	case "reactivate":
		result, err = client.ReactivateMemory(ctx, conn.Endpoint, conn.Token, in)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	return printMemoryMutation(result, *jsonOut)
}

func memoryClientFlags(fs *flag.FlagSet) (*string, *string, *string, *string) {
	runtimeName := fs.String("client-runtime", "", "self-reported client runtime")
	model := fs.String("client-model", "", "self-reported model name")
	recipe := fs.String("client-recipe", "", "self-reported curation or capture recipe")
	recipeVersion := fs.String("client-recipe-version", "", "self-reported recipe version")
	return runtimeName, model, recipe, recipeVersion
}

func memoryClientProvenance(runtimeName, model, recipe, recipeVersion string) client.MemoryClientProvenance {
	return client.MemoryClientProvenance{
		Runtime: runtimeName, Model: model, Recipe: recipe, RecipeVersion: recipeVersion,
	}
}

func memoryLeadingID(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return strings.TrimSpace(args[0]), args[1:]
	}
	return "", args
}

func memoryOptionalContent(inline, file string, stdin bool) (*string, error) {
	provided := strings.TrimSpace(inline) != "" || strings.TrimSpace(file) != "" || stdin
	if !provided {
		return nil, nil
	}
	content, err := readBodyFromFlags(inline, file, stdin)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("replacement content must not be empty")
	}
	return &content, nil
}

func memoryOptionalUnitFloat(value float64) (*float64, error) {
	if value == -1 {
		return nil, nil
	}
	if value < 0 || value > 1 {
		return nil, fmt.Errorf("must be between 0 and 1")
	}
	return &value, nil
}

func normalizeMemoryContentEncoding(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "plain", nil
	}
	if value != "plain" && value != "base64" {
		return "", fmt.Errorf("must be plain or base64")
	}
	return value, nil
}

func memoryTimeRange(fromRaw, untilRaw string) (*time.Time, *time.Time, error) {
	from, err := factTimeFlag(fromRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("--occurred-from %v", err)
	}
	until, err := factTimeFlag(untilRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("--occurred-until %v", err)
	}
	if from != nil && until != nil && until.Before(*from) {
		return nil, nil, fmt.Errorf("range end must not precede range start")
	}
	return from, until, nil
}

type memoryEvidenceFlags struct {
	File              string
	TranscriptID      string
	FromSequence      int64
	UntilSequence     int64
	ExternalLocator   string
	UnavailableReason string
	Role              string
	SourceDigest      string
}

func memoryCaptureEvidence(in memoryEvidenceFlags) ([]client.MemoryEvidenceInput, error) {
	hasStructuredFlags := strings.TrimSpace(in.TranscriptID) != "" || in.FromSequence != 0 ||
		in.UntilSequence != 0 || strings.TrimSpace(in.ExternalLocator) != "" ||
		strings.TrimSpace(in.UnavailableReason) != "" || strings.TrimSpace(in.SourceDigest) != ""
	if strings.TrimSpace(in.File) != "" {
		if hasStructuredFlags {
			return nil, fmt.Errorf("--evidence-file cannot be combined with individual evidence flags")
		}
		raw, err := os.ReadFile(in.File)
		if err != nil {
			return nil, fmt.Errorf("read --evidence-file: %w", err)
		}
		var evidence []client.MemoryEvidenceInput
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&evidence); err != nil {
			return nil, fmt.Errorf("decode --evidence-file: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			if err == nil {
				err = fmt.Errorf("multiple JSON values")
			}
			return nil, fmt.Errorf("decode --evidence-file: %w", err)
		}
		if evidence == nil {
			evidence = []client.MemoryEvidenceInput{}
		}
		if len(evidence) == 0 {
			return nil, fmt.Errorf("--evidence-file must contain at least one exact, pending, or unavailable evidence object")
		}
		return evidence, nil
	}
	modes := 0
	if strings.TrimSpace(in.TranscriptID) != "" || in.FromSequence != 0 || in.UntilSequence != 0 {
		modes++
	}
	if strings.TrimSpace(in.ExternalLocator) != "" {
		modes++
	}
	if strings.TrimSpace(in.UnavailableReason) != "" {
		modes++
	}
	if modes > 1 {
		return nil, fmt.Errorf("resolved transcript, pending locator, and unavailable evidence modes are mutually exclusive")
	}
	if modes == 0 {
		return nil, fmt.Errorf("one of --evidence-transcript, --evidence-locator, --evidence-unavailable-reason, or --evidence-file is required")
	}
	role := strings.TrimSpace(in.Role)
	if role != "supports" && role != "contradicts" && role != "context" {
		return nil, fmt.Errorf("--evidence-role must be supports, contradicts, or context")
	}
	evidence := client.MemoryEvidenceInput{Role: role, SourceDigest: in.SourceDigest}
	switch {
	case strings.TrimSpace(in.TranscriptID) != "" || in.FromSequence != 0 || in.UntilSequence != 0:
		if strings.TrimSpace(in.TranscriptID) == "" || in.FromSequence < 1 || in.UntilSequence < in.FromSequence {
			return nil, fmt.Errorf("resolved transcript evidence requires --evidence-transcript and a positive ordered sequence range")
		}
		evidence.State = "resolved"
		evidence.Type = "transcript"
		evidence.TranscriptID = in.TranscriptID
		from, until := in.FromSequence, in.UntilSequence
		evidence.EntryFromSequence = &from
		evidence.EntryUntilSequence = &until
	case strings.TrimSpace(in.ExternalLocator) != "":
		evidence.State = "pending"
		evidence.Type = "transcript"
		evidence.ExternalLocator = in.ExternalLocator
	case strings.TrimSpace(in.UnavailableReason) != "":
		evidence.State = "unavailable"
		evidence.UnavailableReason = in.UnavailableReason
	}
	return []client.MemoryEvidenceInput{evidence}, nil
}

func printMemoryMutation(result *client.MemoryMutationResult, jsonOut bool) int {
	if result == nil {
		fmt.Fprintln(os.Stderr, "witself: server returned no memory mutation result")
		return 1
	}
	if jsonOut {
		return printJSON(result)
	}
	fmt.Printf("%s\t%d\t%s\t%s\n", result.Memory.ID, result.Memory.Version,
		result.Memory.State, result.Receipt.Operation)
	if result.Receipt.IdempotencyKey != "" {
		fmt.Fprintf(os.Stderr, "idempotency key: %s\n", result.Receipt.IdempotencyKey)
	}
	return 0
}

func printMemory(memory *client.Memory) {
	fmt.Printf("memory:\t%s\n", memory.ID)
	fmt.Printf("version:\t%d\n", memory.Version)
	fmt.Printf("state:\t%s\n", memory.State)
	fmt.Printf("kind:\t%s\n", safeText(memory.Kind))
	fmt.Printf("content encoding:\t%s\n", safeText(memory.ContentEncoding))
	fmt.Printf("salience:\t%.2f\n", memory.Salience)
	fmt.Printf("sensitive:\t%t\n", memory.Sensitive)
	fmt.Printf("tags:\t%s\n", safeText(strings.Join(memory.Tags, ", ")))
	fmt.Printf("updated:\t%s\n", formatTime(memory.UpdatedAt))
	if memory.SupersessionSetID != "" {
		fmt.Printf("supersession set:\t%s\n", safeText(memory.SupersessionSetID))
		fmt.Printf("supersession revision:\t%d\n", memory.SupersessionSetRevision)
		fmt.Printf("supersession replacement count:\t%d\n", memory.SupersessionReplacementCount)
		fmt.Printf("supersession replacement digest:\t%s\n", safeText(memory.SupersessionReplacementDigest))
	}
	if memory.ActiveSupersessionSetID != "" {
		fmt.Printf("active supersession set:\t%s\n", safeText(memory.ActiveSupersessionSetID))
		fmt.Printf("active supersession revision:\t%d\n", memory.ActiveSupersessionSetRevision)
	}
	if memory.OccurredFrom != nil {
		fmt.Printf("occurred from:\t%s\n", formatTime(*memory.OccurredFrom))
	}
	if memory.OccurredUntil != nil {
		fmt.Printf("occurred until:\t%s\n", formatTime(*memory.OccurredUntil))
	}
	if memory.Redacted {
		fmt.Println("content:\t[redacted]")
		return
	}
	fmt.Printf("content:\n%s\n", safeText(memory.Content))
}

func memoryPreview(content string, redacted bool) string {
	if redacted {
		return "[redacted]"
	}
	content = strings.TrimSpace(safeText(content))
	runes := []rune(content)
	if len(runes) <= 120 {
		return content
	}
	return string(runes[:119]) + "…"
}
