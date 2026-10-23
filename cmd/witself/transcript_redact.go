package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/witwave-ai/witself/internal/client"
)

var transcriptRedactionReceiptPattern = regexp.MustCompile(`^trd_[0-9a-f]{32}$`)

func transcriptRedact(args []string) int {
	transcriptID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("transcript redact", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account := accountFlag(fs)
	endpoint := fs.String("endpoint", "", "witself-server endpoint URL")
	tokenFile := fs.String("token-file", "", "file containing an operator token")
	sequence := fs.Int64("sequence", 0, "one entry sequence to redact")
	from := fs.Int64("from-sequence", 0, "first entry sequence to redact, inclusive")
	until := fs.Int64("until-sequence", 0, "last entry sequence to redact, inclusive; at most 100 entries")
	dryRun := fs.Bool("dry-run", false, "preview redaction without changing entries")
	yes := fs.Bool("yes", false, "confirm irreversible owner-authorized redaction")
	revision := fs.String("expected-redaction-set-revision", "", "exact lowercase SHA-256 revision returned by preview")
	key := fs.String("idempotency-key", "", "fresh retry key for this one logical redaction")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if transcriptID == "" && fs.NArg() == 1 {
		transcriptID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		transcriptRedactUsage()
		return 2
	}
	provided := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if provided["sequence"] {
		if provided["from-sequence"] || provided["until-sequence"] {
			transcriptRedactUsage()
			return 2
		}
		*from, *until = *sequence, *sequence
	}
	if transcriptID == "" || *from < 1 || *until < *from || *until-*from >= 100 || (*dryRun && *yes) {
		transcriptRedactUsage()
		return 2
	}
	*revision, *key = strings.TrimSpace(*revision), strings.TrimSpace(*key)
	if *dryRun {
		if provided["expected-redaction-set-revision"] || provided["idempotency-key"] {
			fmt.Fprintln(os.Stderr, "witself: --dry-run accepts no mutation guards")
			return 2
		}
	} else {
		if !*yes {
			fmt.Fprintln(os.Stderr, "witself: transcript redaction is permanent and cannot be undone; preview with --dry-run, then apply the exact guards with --yes")
			return 2
		}
		if !factCandidateRevisionPattern.MatchString(*revision) || len(*key) < 1 || len(*key) > 512 {
			fmt.Fprintln(os.Stderr, "witself: apply requires --yes, a 64-character lowercase --expected-redaction-set-revision, and --idempotency-key of 1-512 bytes")
			return 2
		}
	}
	if *tokenFile != "" && *endpoint == "" {
		fmt.Fprintln(os.Stderr, "witself: --token-file needs --endpoint")
		return 2
	}

	ctx := context.Background()
	ep, token, err := connect(ctx, *account, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %s\n", tabSafe(safeText(err.Error())))
		return 1
	}
	var receipt *client.TranscriptRedactionReceipt
	if *dryRun {
		receipt, err = client.PreviewRedactTranscriptEntries(ctx, ep, token, transcriptID, *from, *until)
	} else {
		receipt, err = client.RedactTranscriptEntries(ctx, ep, token, client.RedactTranscriptEntriesInput{
			TranscriptID: transcriptID, FromSequence: *from, UntilSequence: *until,
			ExpectedRedactionSetRevision: *revision, IdempotencyKey: *key,
		})
	}
	if err != nil {
		if !*dryRun && !transcriptRedactDefiniteRefusal(err) {
			// A lost or unreadable apply response leaves the outcome unknown.
			fmt.Fprintf(os.Stderr, "witself: transcript redaction: %s; the outcome is unknown and the redaction may already be applied; rerun with --dry-run, or retry with the SAME --idempotency-key to replay the receipt\n", tabSafe(safeText(err.Error())))
			return 1
		}
		return transcriptRedactError(err)
	}
	if receipt == nil {
		err = errors.New("server returned no transcript redaction receipt")
	} else if *dryRun {
		err = validateTranscriptRedactionPreview(*receipt, transcriptID, *from, *until)
	} else {
		err = validateTranscriptRedactionApply(*receipt, transcriptID, *from, *until, *revision)
	}
	if err != nil {
		if !*dryRun {
			fmt.Fprintf(os.Stderr, "witself: transcript redaction: %s; the server accepted the request and the redaction may already be applied; rerun with --dry-run, or retry with the SAME --idempotency-key to replay the receipt\n", tabSafe(safeText(err.Error())))
			return 1
		}
		return transcriptRedactError(err)
	}
	if *jsonOut {
		return printJSON(map[string]any{"redaction": receipt})
	}
	printTranscriptRedactionReceipt(*receipt)
	if *dryRun {
		if receipt.RedactableCount == 0 {
			fmt.Println("nothing to redact (every entry in the range is already redacted)")
		} else {
			fmt.Printf("apply command:\n  %s\n", transcriptRedactionApplyCommand(*receipt, *account, *endpoint, *tokenFile))
		}
	}
	return 0
}

func transcriptRedactUsage() {
	fmt.Fprintln(os.Stderr, "usage: witself transcript redact TRANSCRIPT_ID (--sequence N | --from-sequence N --until-sequence M) --dry-run")
	fmt.Fprintln(os.Stderr, "       witself transcript redact TRANSCRIPT_ID (--sequence N | --from-sequence N --until-sequence M) --yes --expected-redaction-set-revision SHA256 --idempotency-key KEY")
}

// transcriptRedactDefiniteRefusal reports whether the server refused the
// request before it could apply anything.
func transcriptRedactDefiniteRefusal(err error) bool {
	for _, target := range []error{client.ErrBadRequest, client.ErrUnauthorized, client.ErrForbidden, client.ErrNotFound, client.ErrConflict, client.ErrFeatureNotEnabled} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func transcriptRedactError(err error) int {
	hint := ""
	if errors.Is(err, client.ErrForbidden) {
		hint = " (transcript redaction needs the account owner's operator token, not an agent token)"
	} else if errors.Is(err, client.ErrNotFound) {
		hint = " (or this server does not support transcript redaction; check features.transcript_redaction)"
	}
	fmt.Fprintf(os.Stderr, "witself: transcript redaction: %s%s\n", tabSafe(safeText(err.Error())), hint)
	return 1
}

func validateTranscriptRedactionPreview(r client.TranscriptRedactionReceipt, id string, from, until int64) error {
	if err := validateTranscriptRedactionSet(r, id, from, until); err != nil {
		return err
	}
	if r.Applied || r.Replayed || r.ReceiptID != "" || r.RedactedAt != nil || r.RedactedCount != 0 || r.CancelledCurationRunCount != 0 {
		return errors.New("server returned an inconsistent transcript redaction preview")
	}
	var already int64
	for _, entry := range r.Entries {
		if entry.Redacted {
			already++
		}
	}
	if already != r.AlreadyRedactedCount {
		return errors.New("server returned inconsistent preview entry counts")
	}
	return nil
}

func validateTranscriptRedactionApply(r client.TranscriptRedactionReceipt, id string, from, until int64, revision string) error {
	if err := validateTranscriptRedactionSet(r, id, from, until); err != nil {
		return err
	}
	if !r.Applied || !transcriptRedactionReceiptPattern.MatchString(r.ReceiptID) || r.RedactedAt == nil ||
		r.RedactedAt.IsZero() || r.RedactionSetRevision != revision || r.RedactedCount < 1 ||
		r.RedactedCount > r.EntryCount || r.RedactedCount != r.RedactableCount {
		return errors.New("server returned an incomplete or inconsistent transcript redaction receipt")
	}
	for _, entry := range r.Entries {
		if !entry.Redacted {
			return errors.New("server returned an unredacted entry after apply")
		}
	}
	return nil
}

func validateTranscriptRedactionSet(r client.TranscriptRedactionReceipt, id string, from, until int64) error {
	if from < 1 || until < from || until-from >= 100 || r.TranscriptID != id || r.FromSequence != from || r.UntilSequence != until ||
		!factCandidateRevisionPattern.MatchString(r.RedactionSetRevision) || r.EntryCount != until-from+1 ||
		r.EntryCount != int64(len(r.Entries)) || r.RedactableCount < 0 || r.RedactableCount > r.EntryCount ||
		r.AlreadyRedactedCount != r.EntryCount-r.RedactableCount || r.MemoryEvidenceCount < 0 ||
		r.CurationRunsToCancel < 0 || r.CancelledCurationRunCount < 0 {
		return errors.New("server returned an incomplete or inconsistent transcript redaction set")
	}
	for i, entry := range r.Entries {
		if entry.Sequence != from+int64(i) {
			return errors.New("server returned noncontiguous transcript redaction entries")
		}
	}
	return nil
}

func printTranscriptRedactionReceipt(r client.TranscriptRedactionReceipt) {
	partial := "no"
	if r.PartialCaptureEvent {
		partial = "yes"
	}
	fmt.Printf("transcript\t%s\nrange\t%d-%d\nentries\t%d\nredactable\t%d\nalready redacted\t%d\nredacted\t%d\npartial capture event\t%s\nmemory evidence\t%d\n",
		tabSafe(safeText(r.TranscriptID)), r.FromSequence, r.UntilSequence, r.EntryCount,
		r.RedactableCount, r.AlreadyRedactedCount, r.RedactedCount, partial, r.MemoryEvidenceCount)
	for _, id := range r.EvidenceMemoryIDs {
		fmt.Printf("evidence memory\t%s\n", tabSafe(safeText(id)))
	}
	if r.Applied {
		fmt.Printf("cancelled curation runs\t%d\nreceipt\t%s\nreplayed\t%t\n", r.CancelledCurationRunCount, r.ReceiptID, r.Replayed)
	} else {
		fmt.Printf("curation runs to cancel\t%d\n", r.CurationRunsToCancel)
	}
	for _, entry := range r.Entries {
		fmt.Printf("seq=%d\trole=%s\tkind=%s\tredacted=%t\n", entry.Sequence, tabSafe(safeText(entry.Role)), tabSafe(safeText(entry.Kind)), entry.Redacted)
	}
	fmt.Printf("redaction set revision\t%s\n", r.RedactionSetRevision)
}

func transcriptRedactionApplyCommand(r client.TranscriptRedactionReceipt, account, endpoint, tokenFile string) string {
	args := []string{"witself", "transcript", "redact", quoteFactDeletionReplayArg(r.TranscriptID),
		"--from-sequence", strconv.FormatInt(r.FromSequence, 10), "--until-sequence", strconv.FormatInt(r.UntilSequence, 10)}
	if account != "" {
		args = append(args, "--account", quoteFactDeletionReplayArg(account))
	}
	if endpoint != "" {
		args = append(args, "--endpoint", quoteFactDeletionReplayArg(endpoint))
	}
	if tokenFile != "" {
		args = append(args, "--token-file", quoteFactDeletionReplayArg("<operator token file>"))
	}
	args = append(args, "--yes", "--expected-redaction-set-revision", quoteFactDeletionReplayArg(r.RedactionSetRevision),
		"--idempotency-key", quoteFactDeletionReplayArg("<fresh key>"))
	return strings.Join(args, " ")
}
