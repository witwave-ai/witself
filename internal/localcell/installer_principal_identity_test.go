package localcell

import (
	"bytes"
	"encoding/base64"
	"os"
	"testing"
)

// TestPrincipalCredentialReplacementBeforeOpen preserves both files and refuses
// the replacement. On Linux and Darwin pathname identity is eager, so refusal
// happens with or without the pin; the source contract holds the pin there. On
// Windows this test fails without the pin because the replacement value is returned.
func TestPrincipalCredentialReplacementBeforeOpen(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	c, m, values := selectedCredentialFixture(t)
	purpose := credentialServer
	path, err := c.credentialPath(purpose)
	if err != nil {
		t.Fatal("credential path was refused")
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("original credential could not be read")
	}
	aside := path + ".inspected"
	value := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{211}, 32))
	for _, fixtureValue := range values {
		if value == fixtureValue {
			t.Fatal("replacement credential must differ from every fixture value")
		}
	}
	replacement := []byte(value + "\n")
	realOpen := openPrincipalCredentialFile
	t.Cleanup(func() { openPrincipalCredentialFile = realOpen })
	opened := false
	openPrincipalCredentialFile = func(name string, flags int, mode os.FileMode) (*os.File, error) {
		if name != path || opened {
			t.Fatal("unexpected principal credential open")
		}
		opened = true
		if err := os.Rename(path, aside); err != nil {
			t.Fatal("original credential could not be moved aside")
		}
		if err := writeAtomicBytes(path, replacement, nil); err != nil {
			t.Fatal("replacement credential could not be published")
		}
		info, err := os.Lstat(path)
		if err != nil || !privatePrincipalCredential(path, info) {
			t.Fatal("replacement credential was not a private regular file")
		}
		return realOpen(name, flags, mode)
	}
	got, err := c.credentialPassword(m, purpose)
	if !opened {
		t.Fatal("principal credential replacement seam was not reached")
	}
	if got != "" || err != ErrLocalPrincipalCredentialsRefused {
		t.Fatal("replaced credential did not return the empty value and exact refusal")
	}
	for name, want := range map[string][]byte{aside: original, path: replacement} {
		data, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(data, want) {
			t.Fatal("refused read changed inspected or replacement credential bytes")
		}
	}
}
