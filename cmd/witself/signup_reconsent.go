package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/signuplegal"
)

// resumeAccountLegalReconsent runs only for a refusal saved by an earlier CLI
// invocation. The pending journal is the user's durable explicit choice; after
// an ambiguous response it is replayed without selecting new versions or IDs.
func resumeAccountLegalReconsent(ctx context.Context, localName, endpoint, email, invite, displayName string,
	acceptTerms bool, journal local.AccountProvisionJournal,
) (local.AccountProvisionJournal, error) {
	return resumeAccountLegalReconsentWithBegin(ctx, localName, endpoint, email, invite, displayName,
		acceptTerms, journal, local.BeginAccountProvisionReconsent)
}

// The per-call publication boundary lets fault tests fail durability before
// any network call, without a mutable global or an alternate production path.
func resumeAccountLegalReconsentWithBegin(ctx context.Context, localName, endpoint, email, invite, displayName string,
	acceptTerms bool, journal local.AccountProvisionJournal,
	begin func(string, local.AccountProvisionJournal, string, string, string) (local.AccountProvisionJournal, error),
) (local.AccountProvisionJournal, error) {
	if journal.LegalRefusal == nil {
		return journal, nil
	}
	if journal.Reconsent == nil {
		if !acceptTerms {
			return journal, errors.New("legal acceptance changed; review the current documents and rerun the same account create command with --accept-terms")
		}
		base, err := signupLegalBase(endpoint)
		if err != nil {
			return journal, err
		}
		terms, privacy, err := signupLegalVersions(ctx, base)
		if err != nil {
			return journal, fmt.Errorf("fetch current legal versions: %w", err)
		}
		fingerprint, err := client.AccountCreateRequestFingerprint(endpoint, localName, email, invite, displayName, terms, privacy)
		if err != nil {
			return journal, err
		}
		fmt.Printf("recording renewed consent to Terms of Service v%s and Privacy Policy v%s\n", terms, privacy)
		fmt.Printf("  %s/terms · %s/privacy\n", base, base)
		journal, err = begin(localName, journal, fingerprint, terms, privacy)
		if err != nil {
			return journal, err
		}
	} else {
		// A previous rename may have succeeded while its directory sync failed.
		// Re-enter the exact saved choice to establish durability before any
		// remote reservation; a Read alone is not a durability acknowledgement.
		var err error
		journal, err = begin(localName, journal,
			journal.Reconsent.RequestFingerprint, journal.Reconsent.Candidate.ConsentTermsVersion,
			journal.Reconsent.Candidate.ConsentPrivacyVersion)
		if err != nil {
			return journal, err
		}
	}
	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	normalizedDisplayName := strings.TrimSpace(displayName)
	if normalizedDisplayName == "" {
		normalizedDisplayName = normalizedEmail
	}
	request := signuplegal.ReconsentRequest{
		SchemaVersion: signuplegal.ReconsentSchema,
		Refusal:       *journal.LegalRefusal,
		TransitionID:  journal.Reconsent.TransitionID,
		Candidate:     journal.Reconsent.Candidate,
		Email:         normalizedEmail,
		Invite:        strings.TrimSpace(invite),
		DisplayName:   normalizedDisplayName,
	}
	ack, err := client.RegisterAccountReconsent(ctx, endpoint, request)
	if err != nil {
		return journal, err
	}
	return local.PromoteAccountProvisionReconsent(localName, journal, *ack)
}
