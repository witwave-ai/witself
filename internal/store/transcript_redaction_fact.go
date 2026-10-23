package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5"
)

var transcriptFactSourceRefPattern = regexp.MustCompile(`^witself://transcript/([^/]+)/entry/([^/]+)$`)

// transcriptRedactionProbeSQL returns only the value-free inputs to the shared
// marker predicate. Its arguments are SQL parameter references supplied by the
// caller, never request values. Unknown keys and kinds must not reach Go.
func transcriptRedactionProbeSQL(placeholderParam, kindsParam string) string {
	return fmt.Sprintf(`
		e.body = %s,
		e.artifacts = '[]'::jsonb,
		CASE WHEN NOT EXISTS (
			SELECT 1 FROM jsonb_object_keys(e.payload) AS k(key)
			WHERE k.key NOT IN ('kind', 'witself_redaction')
		) THEN ARRAY(SELECT k.key FROM jsonb_object_keys(e.payload) AS k(key) ORDER BY k.key COLLATE "C")
		ELSE ARRAY['__non_marker__']::text[] END,
		COALESCE(jsonb_typeof(e.payload->'kind'), ''),
		CASE WHEN jsonb_typeof(e.payload->'kind') = 'string' AND e.payload->>'kind' = ANY(%s::text[])
			THEN e.payload->>'kind' ELSE '' END,
		CASE WHEN e.body = %s AND e.artifacts = '[]'::jsonb THEN e.payload->'witself_redaction' END`,
		placeholderParam, kindsParam, placeholderParam)
}

func transcriptRedactionKindVocabulary() []string {
	kinds := make([]string, 0, len(transcriptCaptureKinds))
	for kind := range transcriptCaptureKinds {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

// Opaque references retain their existing behavior. Only an exact reference
// resolving to this agent's own redacted entry is refused as new fact evidence.
func refuseRedactedTranscriptSourceRefTx(ctx context.Context, tx pgx.Tx, p Principal, ref string) error {
	parts := transcriptFactSourceRefPattern.FindStringSubmatch(ref)
	if parts == nil {
		return nil
	}
	var probe transcriptEntryRedactionProbe
	err := tx.QueryRow(ctx, `SELECT `+transcriptRedactionProbeSQL("$6", "$7")+`
		FROM transcript_entries e
		JOIN transcript_conversations c ON c.id=e.transcript_id AND c.account_id=e.account_id
		WHERE c.account_id=$1 AND c.realm_id=$2 AND c.owner_agent_id=$3
		  AND c.id=$4 AND e.id=$5`,
		p.AccountID, p.RealmID, p.ID, parts[1], parts[2], TranscriptRedactedBody,
		transcriptRedactionKindVocabulary()).Scan(
		&probe.BodyMatches, &probe.ArtifactsEmpty, &probe.PayloadKeys,
		&probe.KindType, &probe.KindText, &probe.Marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, redacted := transcriptEntryRedactionFromProbe(probe); redacted {
		return fmt.Errorf("%w: transcript entry is redacted and cannot be fact evidence", ErrFactInputInvalid)
	}
	return nil
}
