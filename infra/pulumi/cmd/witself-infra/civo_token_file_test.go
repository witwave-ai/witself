package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveCivoTokenFileErrorsHideValue(t *testing.T) {
	const hint = " (path not shown; it comes from -civo-token-file or the cell's security_context.civo.token_file)"
	for _, tt := range []struct {
		name       string
		missing    bool
		directory  bool
		unreadable bool
		content    string
		mode       fs.FileMode
		want       string
	}{
		{
			name:    "missing value",
			missing: true,
			want:    "read Civo token file: no such file or directory" + hint,
		},
		{
			name:      "directory",
			directory: true,
			want:      "civo token file is not a regular file" + hint,
		},
		{
			name:    "oversized file",
			content: strings.Repeat("x", 4097) + "-not-real",
			mode:    0o600,
			want:    "civo token file is unexpectedly large" + hint,
		},
		{
			name:    "broad permissions",
			content: civoTestToken,
			mode:    0o644,
			want:    "civo token file permissions are too broad (want 0600)" + hint,
		},
		{
			name: "empty file",
			mode: 0o600,
			want: "civo token file is empty" + hint,
		},
		{
			name:       "unreadable file",
			unreadable: true,
			content:    civoTestToken,
			mode:       0o600,
			want:       "read Civo token file: permission denied" + hint,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r2TestHome(t)
			tokenFile := filepath.Join(t.TempDir(), "token-file-not-real")
			switch {
			case tt.missing:
				tokenFile = "this-is-a-token-not-a-file"
			case tt.directory:
				if err := os.Mkdir(tokenFile, 0o700); err != nil {
					t.Fatal("create token-directory fixture failed")
				}
			default:
				if err := os.WriteFile(tokenFile, []byte(tt.content), tt.mode); err != nil {
					t.Fatal("write token-file fixture failed")
				}
				if tt.unreadable {
					if err := os.Chmod(tokenFile, 0o200); err != nil {
						t.Fatal("make token-file fixture unreadable failed")
					}
				}
			}
			_, err := resolveCivoToken(tokenFile)
			if err == nil && tt.unreadable && os.Geteuid() == 0 {
				t.Skip("root can read the mode-0200 token-file fixture")
			}
			if err == nil {
				t.Fatal("expected Civo token-file refusal")
			}
			text := err.Error()
			requireNoSecretOutput(t, text)
			if strings.Contains(text, tokenFile) || strings.Contains(text, civoTestToken) {
				t.Error("error disclosed the token-file value or token fixture")
			}
			if !strings.Contains(strings.ToLower(text), "civo token file") {
				t.Error("error lost the dashboard's Civo token-file marker")
			}
			if text != tt.want {
				t.Errorf("error text mismatch; expected %q", tt.want)
			}
			if tt.missing && !errors.Is(err, fs.ErrNotExist) {
				t.Error("missing-file refusal no longer wraps fs.ErrNotExist")
			}
		})
	}
}

func TestCivoTokenFileReadErrorSanitizesReason(t *testing.T) {
	const tokenFile = "test-pasted-civo-token-not-real"
	const hint = " (path not shown; it comes from -civo-token-file or the cell's security_context.civo.token_file)"
	for _, tt := range []struct {
		name    string
		err     error
		want    string
		missing bool
	}{
		{
			name:    "wrapped path error",
			err:     fmt.Errorf("wrapper: %w", &fs.PathError{Op: "stat", Path: tokenFile, Err: fs.ErrNotExist}),
			want:    "read Civo token file: " + fs.ErrNotExist.Error() + hint,
			missing: true,
		},
		{
			name: "non-path error",
			err:  errors.New("cannot read " + tokenFile + "; retried " + tokenFile),
			want: "read Civo token file: cannot read [path not shown]; retried [path not shown]" + hint,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r2TestHome(t)
			err := civoTokenFileReadError(tt.err, tokenFile)
			requireNoSecretOutput(t, err.Error())
			if strings.Contains(err.Error(), tokenFile) {
				t.Error("read error disclosed the token-file value")
			}
			if err.Error() != tt.want {
				t.Errorf("error text mismatch; expected %q", tt.want)
			}
			if tt.missing && !errors.Is(err, fs.ErrNotExist) {
				t.Error("wrapped path error lost fs.ErrNotExist")
			}
		})
	}
}
