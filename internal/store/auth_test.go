package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidateCuratorTokenInput(t *testing.T) {
	tests := []struct {
		name        string
		profile     string
		displayName string
		ttl         time.Duration
		wantName    string
		wantErr     error
	}{
		{
			name: "preview profile", profile: AccessProfileCuratorPreview,
			displayName: "  nightly curator  ", ttl: time.Hour, wantName: "nightly curator",
		},
		{
			name: "apply profile at maximum ttl", profile: AccessProfileCuratorApply,
			displayName: "apply worker", ttl: MaxCuratorTokenTTL, wantName: "apply worker",
		},
		{
			name: "full is not a curator profile", profile: AccessProfileFull,
			displayName: "worker", ttl: time.Hour, wantErr: ErrInvalidCuratorAccessProfile,
		},
		{
			name: "unknown profile", profile: "curator-admin",
			displayName: "worker", ttl: time.Hour, wantErr: ErrInvalidCuratorAccessProfile,
		},
		{
			name: "profile whitespace is not normalized", profile: " curator-preview ",
			displayName: "worker", ttl: time.Hour, wantErr: ErrInvalidCuratorAccessProfile,
		},
		{
			name: "zero ttl", profile: AccessProfileCuratorPreview,
			displayName: "worker", ttl: 0, wantErr: ErrInvalidCuratorTokenTTL,
		},
		{
			name: "negative ttl", profile: AccessProfileCuratorPreview,
			displayName: "worker", ttl: -time.Second, wantErr: ErrInvalidCuratorTokenTTL,
		},
		{
			name: "ttl above maximum", profile: AccessProfileCuratorPreview,
			displayName: "worker", ttl: MaxCuratorTokenTTL + time.Nanosecond, wantErr: ErrInvalidCuratorTokenTTL,
		},
		{
			name: "blank display name", profile: AccessProfileCuratorPreview,
			displayName: " \t ", ttl: time.Hour, wantErr: ErrInvalidCuratorTokenDisplayName,
		},
		{
			name: "oversized display name", profile: AccessProfileCuratorPreview,
			displayName: strings.Repeat("a", maxCuratorTokenDisplayNameBytes+1), ttl: time.Hour,
			wantErr: ErrInvalidCuratorTokenDisplayName,
		},
		{
			name: "control character", profile: AccessProfileCuratorPreview,
			displayName: "worker\nname", ttl: time.Hour, wantErr: ErrInvalidCuratorTokenDisplayName,
		},
		{
			name: "invalid utf8", profile: AccessProfileCuratorPreview,
			displayName: string([]byte{'w', 0xff}), ttl: time.Hour, wantErr: ErrInvalidCuratorTokenDisplayName,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateCuratorTokenInput(tc.profile, tc.displayName, tc.ttl)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want errors.Is(_, %v)", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.wantName {
				t.Fatalf("display name = %q, want %q", got, tc.wantName)
			}
		})
	}
}

func TestValidTokenAccessProfile(t *testing.T) {
	tests := []struct {
		kind, profile string
		want          bool
	}{
		{kind: "bootstrap", profile: AccessProfileFull, want: true},
		{kind: "operator", profile: AccessProfileFull, want: true},
		{kind: "agent", profile: AccessProfileFull, want: true},
		{kind: "agent", profile: AccessProfileCuratorPreview, want: true},
		{kind: "agent", profile: AccessProfileCuratorApply, want: true},
		{kind: "operator", profile: AccessProfileCuratorPreview},
		{kind: "bootstrap", profile: AccessProfileCuratorApply},
		{kind: "agent", profile: ""},
		{kind: "agent", profile: "curator-admin"},
	}
	for _, tc := range tests {
		if got := validTokenAccessProfile(tc.kind, tc.profile); got != tc.want {
			t.Errorf("validTokenAccessProfile(%q, %q) = %t, want %t", tc.kind, tc.profile, got, tc.want)
		}
	}
}
