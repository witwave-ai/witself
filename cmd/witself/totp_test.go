package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/local"
	"github.com/witwave-ai/witself/internal/sealed"
	"github.com/witwave-ai/witself/internal/secretclient"
)

const testTOTPSeed = "JBSWY3DPEHPK3PXP"

type fakeTOTPFieldRevealer struct {
	value                    []byte
	returned                 []byte
	err                      error
	secretID, fieldID, retry string
}

func (f *fakeTOTPFieldRevealer) RevealField(_ context.Context, secretID, fieldID, retry string) ([]byte, error) {
	f.secretID, f.fieldID, f.retry = secretID, fieldID, retry
	f.returned = append([]byte(nil), f.value...)
	return f.returned, f.err
}

func TestTOTPShowCLIPrintsOnlySeedFreeMetadata(t *testing.T) {
	seed := testTOTPSeed
	payload, err := sealed.NewTOTPPayload(seed, sealed.TOTPOptions{
		Algorithm: sealed.TOTPAlgorithmSHA256, Digits: 8, PeriodSeconds: 45,
		Issuer: "Example", Account: "alice@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := sealed.EncodeTOTPPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeTOTPFieldRevealer{value: encoded}
	connection := installFakeTOTPCLI(t, fake)

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		args := append([]string{"totp", "show", "--json", "--idempotency-key", "totp-show-1"}, connection...)
		return run(append(args, "sec_aaaaaaaaaaaaaaaa", "fld_bbbbbbbbbbbbbbbb"))
	})
	if code != 0 || stderr != "" {
		t.Fatalf("run = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, seed) || strings.Contains(stderr, seed) {
		t.Fatalf("show leaked seed: stdout=%q stderr=%q", stdout, stderr)
	}
	var metadata sealed.TOTPPayloadMetadata
	if err := json.Unmarshal([]byte(stdout), &metadata); err != nil {
		t.Fatalf("decode output: %v (%q)", err, stdout)
	}
	if metadata != payload.Metadata() {
		t.Fatalf("metadata = %+v, want %+v", metadata, payload.Metadata())
	}
	if fake.secretID != "sec_aaaaaaaaaaaaaaaa" || fake.fieldID != "fld_bbbbbbbbbbbbbbbb" || fake.retry != "totp-show-1" {
		t.Fatalf("reveal call = %q / %q / %q", fake.secretID, fake.fieldID, fake.retry)
	}
	assertClearedTOTPBytes(t, fake.returned)

	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		// Exercise flags after the two natural positional arguments.
		args := []string{"totp", "show", "sec_aaaaaaaaaaaaaaaa", "fld_bbbbbbbbbbbbbbbb", "--idempotency-key", "totp-show-2"}
		return run(append(args, connection...))
	})
	if code != 0 || stderr != "" {
		t.Fatalf("plain run = %d, stderr = %q", code, stderr)
	}
	want := "issuer\tExample\naccount\talice@example.com\nalgorithm\tSHA256\ndigits\t8\nperiod_seconds\t45\n"
	if stdout != want || strings.Contains(stdout, seed) {
		t.Fatalf("plain output = %q, want %q", stdout, want)
	}
	assertClearedTOTPBytes(t, fake.returned)
}

func TestTOTPCodeCLIUsesInjectedCurrentTimeAndReturnsExpiry(t *testing.T) {
	payload, err := sealed.NewTOTPPayload(testTOTPSeed, sealed.TOTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := sealed.EncodeTOTPPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeTOTPFieldRevealer{value: encoded}
	connection := installFakeTOTPCLI(t, fake)
	fixed := time.Unix(59, 999_999_999)
	previousNow := totpNow
	totpNow = func() time.Time { return fixed }
	t.Cleanup(func() { totpNow = previousNow })
	want, err := sealed.GenerateTOTPCode(payload, fixed)
	if err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		args := append([]string{"totp", "code", "--json"}, connection...)
		return run(append(args, "sec_aaaaaaaaaaaaaaaa", "fld_bbbbbbbbbbbbbbbb"))
	})
	if code != 0 || stderr != "" {
		t.Fatalf("run = %d, stderr = %q", code, stderr)
	}
	var result sealed.TOTPCode
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode output: %v (%q)", err, stdout)
	}
	if result != want || strings.Contains(stdout, testTOTPSeed) {
		t.Fatalf("result = %+v, want %+v", result, want)
	}
	if !strings.HasPrefix(fake.retry, "secret_access_") {
		t.Fatalf("generated retry key = %q", fake.retry)
	}
	assertClearedTOTPBytes(t, fake.returned)

	stdout, stderr, code = captureFactDeleteCLI(t, func() int {
		args := append([]string{"totp", "code", "--idempotency-key", "totp-code-2"}, connection...)
		return run(append(args, "sec_aaaaaaaaaaaaaaaa", "fld_bbbbbbbbbbbbbbbb"))
	})
	if code != 0 || stderr != "" {
		t.Fatalf("plain run = %d, stderr = %q", code, stderr)
	}
	wantPlain := "code\t" + want.Code + "\ndigits\t6\nperiod_seconds\t30\nremaining_seconds\t1\nexpires_at\t1970-01-01T00:01:00Z\n"
	if stdout != wantPlain || strings.Contains(stdout, testTOTPSeed) {
		t.Fatalf("plain output = %q, want %q", stdout, wantPlain)
	}
	assertClearedTOTPBytes(t, fake.returned)
}

func TestTOTPCLIRejectsBadCommandsBeforeReveal(t *testing.T) {
	fake := &fakeTOTPFieldRevealer{}
	connection := installFakeTOTPCLI(t, fake)
	for _, args := range [][]string{
		{"totp"},
		{"totp", "unknown"},
		append([]string{"totp", "show"}, connection...),
		append(append([]string{"totp", "show"}, connection...), "sec_aaaaaaaaaaaaaaaa"),
		append(append([]string{"totp", "code", "--seed", testTOTPSeed}, connection...), "sec_aaaaaaaaaaaaaaaa", "fld_bbbbbbbbbbbbbbbb"),
	} {
		stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(args) })
		if code != 2 || stdout != "" || strings.Contains(stderr, testTOTPSeed) {
			t.Fatalf("run(%q) = %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	if fake.secretID != "" {
		t.Fatalf("invalid command reached reveal: %q", fake.secretID)
	}
}

func TestTOTPCLICollapsesPostRevealErrorsWithoutPlaintext(t *testing.T) {
	for _, test := range []struct {
		name  string
		value []byte
		err   error
	}{
		{name: "reveal", value: []byte(testTOTPSeed), err: errors.New("provider error containing " + testTOTPSeed)},
		{name: "parse", value: []byte(`{"seed_base32":"` + testTOTPSeed + `"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeTOTPFieldRevealer{value: test.value, err: test.err}
			connection := installFakeTOTPCLI(t, fake)
			stdout, stderr, code := captureFactDeleteCLI(t, func() int {
				args := append([]string{"totp", "code"}, connection...)
				return run(append(args, "sec_aaaaaaaaaaaaaaaa", "fld_bbbbbbbbbbbbbbbb"))
			})
			if code != 1 || stdout != "" || strings.Contains(stderr, testTOTPSeed) {
				t.Fatalf("run = %d, stdout=%q stderr=%q", code, stdout, stderr)
			}
			assertClearedTOTPBytes(t, fake.returned)
		})
	}
}

func TestTOTPCLICollapsesVaultInitializationError(t *testing.T) {
	t.Setenv("WITSELF_ACCOUNT", "")
	t.Setenv("WITSELF_REALM", "")
	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_totp_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := connectTOTPFieldRevealer
	connectTOTPFieldRevealer = func(context.Context, string, string, string, string, string) (totpFieldRevealer, error) {
		return nil, errors.New("configuration error containing " + testTOTPSeed)
	}
	t.Cleanup(func() { connectTOTPFieldRevealer = previous })
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{
			"totp", "show", "--endpoint", "https://witself.invalid", "--token-file", tokenFile,
			"--agent", "scott", "sec_aaaaaaaaaaaaaaaa", "fld_bbbbbbbbbbbbbbbb",
		})
	})
	if code != 1 || stdout != "" || strings.Contains(stderr, testTOTPSeed) {
		t.Fatalf("run = %d, stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestTOTPCLIReportsValueFreeKeyStateErrors(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{err: secretclient.ErrKeyUnavailable, want: "local agent vault key is unavailable"},
		{err: secretclient.ErrKeyMismatch, want: "does not match the backend binding"},
		{err: secretclient.ErrIdentityMismatch, want: "does not match the local vault selectors"},
		{err: secretclient.ErrIntegrity, want: "failed integrity verification"},
		{err: secretclient.ErrInvalidInput, want: "secret or field identifier is invalid"},
	} {
		stdout, stderr, code := captureFactDeleteCLI(t, func() int {
			return printTOTPValueError("test operation", test.err)
		})
		if code != 1 || stdout != "" || !strings.Contains(stderr, test.want) {
			t.Fatalf("error %v = code %d, stdout=%q stderr=%q", test.err, code, stdout, stderr)
		}
	}
}

func TestTOTPCLIRequiresLocalAgentSelectorWithExplicitToken(t *testing.T) {
	t.Setenv("WITSELF_AGENT", "")
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_totp_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	previous := connectTOTPFieldRevealer
	connectTOTPFieldRevealer = func(context.Context, string, string, string, string, string) (totpFieldRevealer, error) {
		called = true
		return nil, errors.New("unexpected")
	}
	t.Cleanup(func() { connectTOTPFieldRevealer = previous })
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{
			"totp", "show", "--endpoint", "https://witself.invalid", "--token-file", tokenFile,
			"sec_aaaaaaaaaaaaaaaa", "fld_bbbbbbbbbbbbbbbb",
		})
	})
	if code != 2 || stdout != "" || called || !strings.Contains(stderr, "--agent") {
		t.Fatalf("run = %d, stdout=%q stderr=%q called=%t", code, stdout, stderr, called)
	}
}

func TestConnectSecretClientTOTPFieldRevealerUsesSharedSecretSelectors(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	if err := local.Save("default", local.Account{ID: "acc_aaaaaaaaaaaaaaaa"}, "operator-token"); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_totp_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	revealer, err := connectSecretClientTOTPFieldRevealer(
		context.Background(), "default", "default", "scott", "https://witself.invalid", tokenFile,
	)
	if err != nil || revealer == nil {
		t.Fatalf("connector = %T, %v", revealer, err)
	}
	if _, err := connectSecretClientTOTPFieldRevealer(
		context.Background(), "default", "default", "scott", "", tokenFile,
	); err == nil {
		t.Fatal("connector accepted an explicit token without an endpoint")
	}
}

func installFakeTOTPCLI(t *testing.T, fake *fakeTOTPFieldRevealer) []string {
	t.Helper()
	t.Setenv("WITSELF_ACCOUNT", "")
	t.Setenv("WITSELF_REALM", "")
	previous := connectTOTPFieldRevealer
	connectTOTPFieldRevealer = func(_ context.Context, account, realm, agent, endpoint, tokenFile string) (totpFieldRevealer, error) {
		if account != "default" || realm != "default" || agent != "scott" ||
			endpoint != "https://witself.invalid" || tokenFile == "" {
			t.Fatalf("connection = %q / %q / %q / %q / %q", account, realm, agent, endpoint, tokenFile)
		}
		return fake, nil
	}
	t.Cleanup(func() { connectTOTPFieldRevealer = previous })

	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_totp_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"--endpoint", "https://witself.invalid", "--token-file", tokenFile,
		"--agent", "scott",
	}
}

func assertClearedTOTPBytes(t *testing.T, value []byte) {
	t.Helper()
	for _, b := range value {
		if b != 0 {
			t.Fatal("decrypted TOTP buffer was not cleared")
		}
	}
}
