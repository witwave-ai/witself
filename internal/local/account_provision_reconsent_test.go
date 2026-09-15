package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/signuplegal"
)

func beginLegalJournal(t *testing.T) AccountProvisionJournal {
	t.Helper()
	t.Setenv("WITSELF_HOME", privateAccountProvisionTestHome(t))
	x, _, err := BeginAccountProvisionJournalWithConsent("default", testAccountProvisionFingerprint, "terms-old", "privacy-old")
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func legalRefusalFor(x AccountProvisionJournal) signuplegal.Refusal {
	return signuplegal.Refusal{
		SchemaVersion: signuplegal.RefusalSchema, Code: signuplegal.RefusalCode, Error: signuplegal.RefusalError,
		ProvisionID: x.ProvisionID, RequestFingerprint: strings.Repeat("b", 64),
		ConsentTermsVersion: x.AcceptedTermsVersion, ConsentPrivacyVersion: x.AcceptedPrivacyVersion,
		RefusalID: "refusal_one", RefusalRevision: 1,
		RequiredTermsVersion: "terms-current", RequiredPrivacyVersion: "privacy-current",
	}
}

func refuseLegalJournal(t *testing.T, x AccountProvisionJournal) AccountProvisionJournal {
	t.Helper()
	out, err := RecordAccountProvisionLegalRefusal("default", x, legalRefusalFor(x))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func prepareLegalJournal(t *testing.T, x AccountProvisionJournal) AccountProvisionJournal {
	t.Helper()
	out, err := BeginAccountProvisionReconsent("default", x, testOtherAccountProvisionFingerprint, "terms-current", "privacy-current")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func legalAckFor(x AccountProvisionJournal) signuplegal.Ack {
	return signuplegal.Ack{
		SchemaVersion: signuplegal.AckSchema, Status: "registered", Refusal: *x.LegalRefusal,
		TransitionID: x.Reconsent.TransitionID, Candidate: x.Reconsent.Candidate,
		CandidateRequestFingerprint: strings.Repeat("c", 64),
	}
}

func assertLegalJournal(t *testing.T, want AccountProvisionJournal) {
	t.Helper()
	got, err := ReadAccountProvisionJournal("default")
	if err != nil || !equalAccountProvisionJournal(got, want) {
		t.Fatalf("journal differs; read error=%v", err)
	}
}

func TestAccountProvisionLegalRefusalDurableReplayAndFullFence(t *testing.T) {
	original := beginLegalJournal(t)
	refusal := legalRefusalFor(original)
	first := refuseLegalJournal(t, original)
	if first.SchemaVersion != accountProvisionLegalJournalSchema || first.RequestFingerprint != original.RequestFingerprint ||
		first.ProvisionID != original.ProvisionID || first.Reconsent != nil || *first.LegalRefusal != refusal {
		t.Fatal("refusal did not preserve the exact original attempt")
	}
	assertLegalJournal(t, first)
	for _, expected := range []AccountProvisionJournal{original, first} {
		got, err := RecordAccountProvisionLegalRefusal("default", expected, refusal)
		if err != nil || !equalAccountProvisionJournal(got, first) {
			t.Fatalf("exact refusal replay: %v", err)
		}
	}
	changed := refusal
	changed.RefusalID = "different_refusal"
	if _, err := RecordAccountProvisionLegalRefusal("default", original, changed); !errors.Is(err, ErrAccountProvisionJournalConflict) {
		t.Fatalf("conflicting receipt: %v", err)
	}
	stale := original
	stale.RequestFingerprint = testOtherAccountProvisionFingerprint
	if _, err := RecordAccountProvisionLegalRefusal("default", stale, refusal); !errors.Is(err, ErrAccountProvisionJournalConflict) {
		t.Fatalf("changed full local request: %v", err)
	}
	assertLegalJournal(t, first)
}

func TestAccountProvisionReconsentElectsOneCandidateAndReplaysLostAck(t *testing.T) {
	refused := refuseLegalJournal(t, beginLegalJournal(t))
	const callers = 8
	var wg sync.WaitGroup
	rows := make([]AccountProvisionJournal, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Go(func() {
			rows[i], errs[i] = BeginAccountProvisionReconsent("default", refused, testOtherAccountProvisionFingerprint, "terms-current", "privacy-current")
		})
	}
	wg.Wait()
	for i := range callers {
		if errs[i] != nil || !equalAccountProvisionJournal(rows[0], rows[i]) {
			t.Fatalf("candidate election %d: %v", i, errs[i])
		}
	}
	pending := rows[0]
	if pending.Reconsent == nil || pending.Reconsent.Candidate.ProvisionID == refused.ProvisionID ||
		pending.Reconsent.TransitionID == "" || pending.ProvisionID != refused.ProvisionID {
		t.Fatal("candidate did not retain predecessor and distinct durable identifiers")
	}
	for _, expected := range []AccountProvisionJournal{refused, pending} {
		got, err := BeginAccountProvisionReconsent("default", expected, testOtherAccountProvisionFingerprint, "terms-current", "privacy-current")
		if err != nil || !equalAccountProvisionJournal(got, pending) {
			t.Fatalf("candidate lost-ack replay: %v", err)
		}
	}
	for _, args := range [][3]string{
		{strings.Repeat("a", 64), "terms-current", "privacy-current"},
		{testOtherAccountProvisionFingerprint, "terms-later", "privacy-current"},
		{testOtherAccountProvisionFingerprint, "terms-current", "privacy-later"},
	} {
		if _, err := BeginAccountProvisionReconsent("default", refused, args[0], args[1], args[2]); !errors.Is(err, ErrAccountProvisionJournalConflict) {
			t.Fatalf("changed candidate request: %v", err)
		}
	}
	assertLegalJournal(t, pending)
}

func TestAccountProvisionReconsentPromotionRequiresExactAckAndHasConstantSize(t *testing.T) {
	pending := prepareLegalJournal(t, refuseLegalJournal(t, beginLegalJournal(t)))
	ack := legalAckFor(pending)
	changes := []func(*signuplegal.Ack){
		func(a *signuplegal.Ack) { a.Refusal.RefusalID = "other" },
		func(a *signuplegal.Ack) { a.Refusal.RefusalRevision++ },
		func(a *signuplegal.Ack) { a.Refusal.RequestFingerprint = strings.Repeat("d", 64) },
		func(a *signuplegal.Ack) { a.Candidate.ProvisionID = "another_candidate" },
		func(a *signuplegal.Ack) { a.Candidate.ConsentTermsVersion = "terms-later" },
		func(a *signuplegal.Ack) { a.TransitionID = "other_transition" },
	}
	for _, change := range changes {
		bad := ack
		change(&bad)
		if _, err := PromoteAccountProvisionReconsent("default", pending, bad); !errors.Is(err, ErrAccountProvisionJournalConflict) {
			t.Fatalf("mismatched ack: %v", err)
		}
		assertLegalJournal(t, pending)
	}
	promoted, err := PromoteAccountProvisionReconsent("default", pending, ack)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.SchemaVersion != accountProvisionJournalSchema || promoted.ProvisionID != pending.Reconsent.Candidate.ProvisionID ||
		promoted.RequestFingerprint != pending.Reconsent.RequestFingerprint || promoted.LegalRefusal != nil || promoted.Reconsent != nil {
		t.Fatal("promotion retained lineage or substituted the CP fingerprint")
	}
	if promoted.RequestFingerprint == ack.CandidateRequestFingerprint {
		t.Fatal("CP and local fingerprints conflated")
	}
	replayed, err := PromoteAccountProvisionReconsent("default", pending, ack)
	if err != nil || !equalAccountProvisionJournal(replayed, promoted) {
		t.Fatalf("promotion lost-ack replay: %v", err)
	}
	for range 5 {
		refused := refuseLegalJournal(t, promoted)
		pending = prepareLegalJournal(t, refused)
		promoted, err = PromoteAccountProvisionReconsent("default", pending, legalAckFor(pending))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(promoted)
		if err != nil || len(raw) > 512 || bytes.Contains(raw, []byte("legal_refusal")) || bytes.Contains(raw, []byte("reconsent")) {
			t.Fatal("journal grew with lineage")
		}
	}
	assertLegalJournal(t, promoted)
}

func TestAccountProvisionReconsentCredentialWinnerPreserved(t *testing.T) {
	for _, phase := range []string{"original", "refused", "pending"} {
		t.Run(phase, func(t *testing.T) {
			original := beginLegalJournal(t)
			x := original
			if phase != "original" {
				x = refuseLegalJournal(t, x)
			}
			if phase == "pending" {
				x = prepareLegalJournal(t, x)
			}
			if err := SaveAccountProvisionCredential("default", original.RequestFingerprint, original.ProvisionID, testProvisionAccountID, testProvisionOperatorToken); err != nil {
				t.Fatal(err)
			}
			winner, err := ReadAccountProvisionJournal("default")
			if err != nil || winner.SchemaVersion != accountProvisionJournalSchema || winner.LegalRefusal != nil || winner.Reconsent != nil ||
				winner.AccountID != testProvisionAccountID || winner.OperatorToken != testProvisionOperatorToken {
				t.Fatalf("credential winner lost: %v", err)
			}
			if _, err := RecordAccountProvisionLegalRefusal("default", original, legalRefusalFor(original)); !errors.Is(err, ErrAccountProvisionJournalConflict) {
				t.Fatalf("stale refusal: %v", err)
			}
			if phase != "original" {
				if _, err := BeginAccountProvisionReconsent("default", x, testOtherAccountProvisionFingerprint, "terms-current", "privacy-current"); !errors.Is(err, ErrAccountProvisionJournalConflict) {
					t.Fatalf("stale selection: %v", err)
				}
			}
			if phase == "pending" {
				if _, err := PromoteAccountProvisionReconsent("default", x, legalAckFor(x)); !errors.Is(err, ErrAccountProvisionJournalConflict) {
					t.Fatalf("stale promotion: %v", err)
				}
			}
			assertLegalJournal(t, winner)
		})
	}
}

func TestAccountProvisionLegalJournalStrictReaderAndOldReaderRefusal(t *testing.T) {
	pending := prepareLegalJournal(t, refuseLegalJournal(t, beginLegalJournal(t)))
	path, _ := AccountProvisionJournalPath("default")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// This is the old v1 decoder's seven-field shape and schema boundary.
	var old struct {
		SchemaVersion          string `json:"schema_version"`
		RequestFingerprint     string `json:"request_fingerprint"`
		ProvisionID            string `json:"provision_id"`
		AcceptedTermsVersion   string `json:"accepted_terms_version,omitempty"`
		AcceptedPrivacyVersion string `json:"accepted_privacy_version,omitempty"`
		AccountID              string `json:"account_id,omitempty"`
		OperatorToken          string `json:"operator_token,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&old); err == nil && old.SchemaVersion == accountProvisionJournalSchema {
		t.Fatal("old reader accepted v2 transition")
	}
	var nullPending map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nullPending); err != nil {
		t.Fatal(err)
	}
	nullPending["reconsent"] = json.RawMessage("null")
	nullRaw, err := json.Marshal(nullPending)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"duplicate root":    bytes.Replace(raw, []byte(`"schema_version":`), []byte(`"schema_version":"witself.account-provision-journal.v2","schema_version":`), 1),
		"escaped duplicate": bytes.Replace(raw, []byte(`"transition_id":`), []byte(`"\u0074ransition_id":"other","transition_id":`), 1),
		"case alias":        bytes.Replace(raw, []byte(`"transition_id":`), []byte(`"TRANSITION_ID":`), 1),
		"trailing":          append(append([]byte(nil), raw...), []byte(`{}`)...),
		"truncated":         raw[:len(raw)-2],
		"oversized":         append(append([]byte(nil), raw...), bytes.Repeat([]byte(" "), maxAccountProvisionJournalBytes)...),
		"unknown nested":    bytes.Replace(raw, []byte(`"candidate":{`), []byte(`"candidate":{"untrusted":true,`), 1),
		"null transition":   nullRaw,
		"bad UTF8":          append(append([]byte(nil), raw...), 0xff),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadAccountProvisionJournal("default"); err == nil {
				t.Fatal("unsafe JSON accepted")
			}
		})
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	assertLegalJournal(t, pending)
}

func TestAccountProvisionLegalJournalRejectsMalformedStoredStates(t *testing.T) {
	pending := prepareLegalJournal(t, refuseLegalJournal(t, beginLegalJournal(t)))
	cases := []func(*AccountProvisionJournal){
		func(x *AccountProvisionJournal) { x.SchemaVersion = accountProvisionJournalSchema },
		func(x *AccountProvisionJournal) { x.LegalRefusal = nil },
		func(x *AccountProvisionJournal) { x.Reconsent.Candidate.ProvisionID = x.ProvisionID },
		func(x *AccountProvisionJournal) { x.Reconsent.RequestFingerprint = "bad" },
		func(x *AccountProvisionJournal) { x.LegalRefusal.ConsentTermsVersion = "wrong" },
		func(x *AccountProvisionJournal) {
			x.AccountID = testProvisionAccountID
			x.OperatorToken = testProvisionOperatorToken
		},
	}
	path, _ := AccountProvisionJournalPath("default")
	for _, change := range cases {
		x := pending
		refusal, candidate := *pending.LegalRefusal, *pending.Reconsent
		x.LegalRefusal, x.Reconsent = &refusal, &candidate
		change(&x)
		raw, err := json.Marshal(x)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadAccountProvisionJournal("default"); !errors.Is(err, ErrAccountProvisionJournalInvalid) {
			t.Fatalf("malformed transition: %v", err)
		}
	}
}

func TestAccountProvisionLegalJournalPublicationFaultBoundaries(t *testing.T) {
	for _, phase := range []string{"refusal", "candidate", "promotion"} {
		for _, boundary := range []string{"before file sync", "lost file sync ack", "before rename", "lost rename ack", "before directory sync", "lost directory sync ack"} {
			t.Run(phase+"/"+boundary, func(t *testing.T) {
				original := beginLegalJournal(t)
				before := original
				var desired AccountProvisionJournal
				var retry func() (AccountProvisionJournal, error)
				switch phase {
				case "refusal":
					desired = refuseLegalJournal(t, before)
					retry = func() (AccountProvisionJournal, error) {
						return RecordAccountProvisionLegalRefusal("default", before, *desired.LegalRefusal)
					}
				case "candidate":
					before = refuseLegalJournal(t, before)
					desired = prepareLegalJournal(t, before)
					retry = func() (AccountProvisionJournal, error) {
						return BeginAccountProvisionReconsent("default", before, testOtherAccountProvisionFingerprint, "terms-current", "privacy-current")
					}
				case "promotion":
					before = prepareLegalJournal(t, refuseLegalJournal(t, before))
					var err error
					desired, err = PromoteAccountProvisionReconsent("default", before, legalAckFor(before))
					if err != nil {
						t.Fatal(err)
					}
					retry = func() (AccountProvisionJournal, error) {
						return PromoteAccountProvisionReconsent("default", before, legalAckFor(before))
					}
				}
				home, path, err := accountProvisionJournalLocation("default")
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(before)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				lock, err := acquireAccountProvisionJournalLock(home, filepath.Dir(path))
				if err != nil {
					t.Fatal(err)
				}
				calls := []string{}
				injected := errors.New("injected private publication failure")
				fs := accountProvisionJournalIO{
					syncFile: func(f *os.File) error {
						calls = append(calls, "file sync")
						if boundary == "before file sync" {
							return injected
						}
						if err := f.Sync(); err != nil {
							return err
						}
						if boundary == "lost file sync ack" {
							return injected
						}
						return nil
					},
					rename: func(from, to string) error {
						calls = append(calls, "rename")
						if boundary == "before rename" {
							return injected
						}
						if err := os.Rename(from, to); err != nil {
							return err
						}
						if boundary == "lost rename ack" {
							return injected
						}
						return nil
					},
					syncDirectory: func(directory string) error {
						calls = append(calls, "directory sync")
						if boundary == "before directory sync" {
							return injected
						}
						if err := syncAccountProvisionJournalDirectory(directory); err != nil {
							return err
						}
						return injected
					},
				}
				err = publishAccountProvisionJournalWithIO(home, path, desired, &before, fs)
				lock.release()
				if !errors.Is(err, ErrAccountProvisionJournalStorage) {
					t.Fatalf("fault result: %v", err)
				}
				wantCalls := "file sync,rename,directory sync"
				if boundary == "before file sync" || boundary == "lost file sync ack" {
					wantCalls = "file sync"
				}
				if boundary == "before rename" || boundary == "lost rename ack" {
					wantCalls = "file sync,rename"
				}
				if strings.Join(calls, ",") != wantCalls {
					t.Fatalf("publication ordering = %v", calls)
				}
				published := boundary != "before file sync" && boundary != "lost file sync ack" && boundary != "before rename"
				if published {
					assertLegalJournal(t, desired)
				} else {
					assertLegalJournal(t, before)
				}
				got, err := retry()
				if err != nil {
					t.Fatalf("restart replay: %v", err)
				}
				if published && !equalAccountProvisionJournal(got, desired) {
					t.Fatal("restart changed the durable winner")
				}
				assertLegalJournal(t, got)
				leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".account-provision-*.tmp"))
				if err != nil || len(leftovers) != 0 {
					t.Fatal("publication temporary file remains")
				}
			})
		}
	}
}

func TestAccountProvisionBeginPreservesRefusedAndPendingBytes(t *testing.T) {
	original := beginLegalJournal(t)
	refused := refuseLegalJournal(t, original)
	for _, record := range []AccountProvisionJournal{refused, prepareLegalJournal(t, refused)} {
		path, _ := AccountProvisionJournalPath("default")
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		got, created, err := BeginAccountProvisionJournalWithConsent("default", original.RequestFingerprint, "terms-old", "privacy-old")
		if err != nil || created || !equalAccountProvisionJournal(got, record) {
			t.Fatalf("v2 ordinary recovery: %v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatal("ordinary Begin rewrote the v2 journal")
		}
	}
}

func TestAccountProvisionLegalJournalPublicationCASIncludesNestedFences(t *testing.T) {
	for _, field := range []string{"refusal", "pending"} {
		t.Run(field, func(t *testing.T) {
			before := prepareLegalJournal(t, refuseLegalJournal(t, beginLegalJournal(t)))
			next := AccountProvisionJournal{SchemaVersion: accountProvisionJournalSchema,
				RequestFingerprint: before.Reconsent.RequestFingerprint, ProvisionID: before.Reconsent.Candidate.ProvisionID,
				AcceptedTermsVersion: "terms-current", AcceptedPrivacyVersion: "privacy-current"}
			home, path, err := accountProvisionJournalLocation("default")
			if err != nil {
				t.Fatal(err)
			}
			other := before
			refusal, pending := *before.LegalRefusal, *before.Reconsent
			other.LegalRefusal, other.Reconsent = &refusal, &pending
			if field == "refusal" {
				other.LegalRefusal.RefusalRevision++
			} else {
				other.Reconsent.TransitionID = "different_transition"
			}
			raw, err := json.Marshal(other)
			if err != nil {
				t.Fatal(err)
			}
			lock, err := acquireAccountProvisionJournalLock(home, filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			renamed := false
			fs := accountProvisionJournalIO{
				syncFile: func(f *os.File) error {
					if err := f.Sync(); err != nil {
						return err
					}
					return os.WriteFile(path, raw, 0o600)
				},
				rename:        func(from, to string) error { renamed = true; return os.Rename(from, to) },
				syncDirectory: syncAccountProvisionJournalDirectory,
			}
			err = publishAccountProvisionJournalWithIO(home, path, next, &before, fs)
			lock.release()
			if !errors.Is(err, ErrAccountProvisionJournalConflict) || renamed {
				t.Fatalf("nested CAS lost: %v", err)
			}
			assertLegalJournal(t, other)
		})
	}
}

func TestAccountProvisionLegalJournalRejectsInvalidInputsWithoutMutation(t *testing.T) {
	original := beginLegalJournal(t)
	for _, mutate := range []func(*signuplegal.Refusal){
		func(r *signuplegal.Refusal) { r.ProvisionID = "wrong" },
		func(r *signuplegal.Refusal) { r.ConsentPrivacyVersion = "wrong" },
		func(r *signuplegal.Refusal) { r.RefusalRevision = 0 },
		func(r *signuplegal.Refusal) { r.Error = "private untrusted message" },
	} {
		bad := legalRefusalFor(original)
		mutate(&bad)
		if _, err := RecordAccountProvisionLegalRefusal("default", original, bad); !errors.Is(err, ErrAccountProvisionJournalInvalid) {
			t.Fatalf("invalid refusal: %v", err)
		}
		assertLegalJournal(t, original)
	}
	refused := refuseLegalJournal(t, original)
	for _, args := range [][3]string{{"invalid", "terms", "privacy"}, {testOtherAccountProvisionFingerprint, "", ""}, {testOtherAccountProvisionFingerprint, "terms", ""}} {
		if _, err := BeginAccountProvisionReconsent("default", refused, args[0], args[1], args[2]); !errors.Is(err, ErrAccountProvisionJournalInvalid) {
			t.Fatalf("invalid selection: %v", err)
		}
		assertLegalJournal(t, refused)
	}
	pending := prepareLegalJournal(t, refused)
	for _, mutate := range []func(*signuplegal.Ack){
		func(a *signuplegal.Ack) { a.Status = "pending" },
		func(a *signuplegal.Ack) { a.SchemaVersion = "unknown" },
		func(a *signuplegal.Ack) { a.CandidateRequestFingerprint = "malformed" },
	} {
		bad := legalAckFor(pending)
		mutate(&bad)
		if _, err := PromoteAccountProvisionReconsent("default", pending, bad); !errors.Is(err, ErrAccountProvisionJournalInvalid) {
			t.Fatalf("invalid acknowledgement: %v", err)
		}
		assertLegalJournal(t, pending)
	}
}

func TestAccountProvisionBeginResyncsAmbiguousPromotionBeforeReturningCandidate(t *testing.T) {
	pending := prepareLegalJournal(t, refuseLegalJournal(t, beginLegalJournal(t)))
	candidate := AccountProvisionJournal{
		SchemaVersion: accountProvisionJournalSchema, ProvisionID: pending.Reconsent.Candidate.ProvisionID,
		RequestFingerprint:     pending.Reconsent.RequestFingerprint,
		AcceptedTermsVersion:   pending.Reconsent.Candidate.ConsentTermsVersion,
		AcceptedPrivacyVersion: pending.Reconsent.Candidate.ConsentPrivacyVersion,
	}
	home, path, err := accountProvisionJournalLocation("default")
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(path)
	lock, err := acquireAccountProvisionJournalLock(home, directory)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected promotion directory sync failure")
	fs := accountProvisionJournalIO{syncFile: (*os.File).Sync, rename: os.Rename, syncDirectory: func(string) error { return injected }}
	err = publishAccountProvisionJournalWithIO(home, path, candidate, &pending, fs)
	lock.release()
	if !errors.Is(err, ErrAccountProvisionJournalStorage) {
		t.Fatalf("promotion publication fault: %v", err)
	}
	assertLegalJournal(t, candidate)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	for _, fail := range []bool{true, false} {
		got, created, err := beginAccountProvisionJournalWithConsent("default", candidate.RequestFingerprint,
			candidate.AcceptedTermsVersion, candidate.AcceptedPrivacyVersion, func(dir string) error {
				attempts++
				if dir != directory {
					t.Fatal("sync used the wrong journal directory")
				}
				if fail {
					return injected
				}
				return syncAccountProvisionJournalDirectory(dir)
			})
		if created {
			t.Fatal("recovery created another candidate")
		}
		if fail {
			if !errors.Is(err, ErrAccountProvisionJournalStorage) || got.ProvisionID != "" {
				t.Fatalf("unsynced candidate returned for remote use: %v", err)
			}
		} else if err != nil || !equalAccountProvisionJournal(got, candidate) {
			t.Fatalf("durable replay failed: %v", err)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatal("replay changed the promoted journal bytes")
		}
	}
	if attempts != 2 {
		t.Fatalf("recovery sync attempts = %d", attempts)
	}
	got, created, err := BeginAccountProvisionJournalWithConsent("default", candidate.RequestFingerprint, candidate.AcceptedTermsVersion, candidate.AcceptedPrivacyVersion)
	if err != nil || created || !equalAccountProvisionJournal(got, candidate) {
		t.Fatalf("public ordinary replay: %v", err)
	}
}
