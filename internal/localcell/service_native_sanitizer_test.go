package localcell_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// Exercise the callback used by the proof: testing the regexp alone would miss
// a host-exit error that bypasses sanitization while the stderr tail stays safe.
func TestNativeServiceHostExitDiagnosticSanitizer(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	root, home := t.TempDir(), t.TempDir()
	assertMasked := func(t *testing.T, value, want string) {
		t.Helper()
		var logged strings.Builder
		diagnostics := nativeServiceFailureDiagnostic{
			goos: "windows", root: root, home: home,
			logf:           func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) },
			run:            func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
			foregroundExit: &nativeServiceDiagnosticExit{},
		}
		diagnostics.foregroundExit.complete(errors.New(value))
		logFailure := diagnostics.logFailure
		logFailure(errors.New("fixture failure"), nil)
		const prefix = "native service host exit error: "
		for _, line := range strings.Split(logged.String(), "\n") {
			if strings.HasPrefix(line, prefix) {
				if line != prefix+want {
					t.Fatal("host-exit diagnostic retained a private URL, password, or labelled PID")
				}
				return
			}
		}
		t.Fatal("host-exit diagnostic omitted the sanitized exit error")
	}
	checkURL := func(t *testing.T, dsn, password string) {
		t.Helper()
		parsed, err := url.Parse(dsn)
		if err != nil || parsed.User == nil || parsed.Host != "synthetic-host.invalid" {
			t.Fatal("synthetic URL fixture does not reach the intended authority")
		}
		if got, present := parsed.User.Password(); !present || got != password {
			t.Fatal("synthetic URL fixture does not retain the intended password")
		}
		assertMasked(t, dsn, "<dsn>")
	}

	t.Run("apostrophe_authority_and_path", func(t *testing.T) {
		checkURL(t, "postgres://fixture:synthetic'password@synthetic-host.invalid/private'database", "synthetic'password")
	})
	t.Run("percent_encoded_password", func(t *testing.T) {
		checkURL(t, "postgresql://fixture:synthetic%27%40%2Fpassword@synthetic-host.invalid/private%27database", "synthetic'@/password")
	})
	t.Run("accepted_raw_password_bytes", func(t *testing.T) {
		accepted := 0
		for b := 0; b < 128; b++ {
			password := "synthetic" + string(rune(b)) + "password"
			dsn := "postgres://fixture:" + password + "@synthetic-host.invalid/private-database"
			parsed, err := url.Parse(dsn)
			if err != nil || parsed.User == nil || parsed.Host != "synthetic-host.invalid" {
				continue
			}
			if got, present := parsed.User.Password(); !present || got != password {
				continue
			}
			accepted++
			t.Run(fmt.Sprintf("byte_%02x", b), func(t *testing.T) { checkURL(t, dsn, password) })
		}
		if accepted == 0 {
			t.Fatal("no Go URL password bytes were exercised")
		}
	})
	t.Run("percent_encoded_password_bytes", func(t *testing.T) {
		for b := 0; b < 256; b++ {
			password := "synthetic" + string([]byte{byte(b)}) + "password"
			dsn := "postgres://fixture:synthetic" + fmt.Sprintf("%%%02X", b) + "password@synthetic-host.invalid/private-database"
			t.Run(fmt.Sprintf("byte_%02x", b), func(t *testing.T) { checkURL(t, dsn, password) })
		}
	})
	t.Run("accepted_path_delimiters", func(t *testing.T) {
		// URL paths may contain unescaped whitespace and quotes. Their end is
		// not safely inferable from a diagnostic line, so redact its remainder.
		for index, path := range []string{"/private'database", "/private\"database", "/private<database>", "/private database trailing-data"} {
			t.Run(fmt.Sprintf("path_%d", index), func(t *testing.T) {
				checkURL(t, "postgres://fixture:synthetic-password@synthetic-host.invalid"+path, "synthetic-password")
			})
		}
	})
	t.Run("password_assignments", func(t *testing.T) {
		for index, value := range []string{"password=synthetic-password", "password='synthetic quoted password'", `password="synthetic quoted password"`, `password='synthetic\'password'`, "PGPASSWORD=synthetic-password", "pgpassword = 'synthetic quoted password'"} {
			t.Run(fmt.Sprintf("assignment_%d", index), func(t *testing.T) { assertMasked(t, value, "<password>") })
		}
	})
	t.Run("labelled_pids", func(t *testing.T) {
		for index, value := range []string{"pid=914723", "PID: 914723", "pid 914723", "host_pid = 914723", "postmaster_pid:914723"} {
			t.Run(fmt.Sprintf("pid_%d", index), func(t *testing.T) { assertMasked(t, value, "<pid>") })
		}
	})
}
