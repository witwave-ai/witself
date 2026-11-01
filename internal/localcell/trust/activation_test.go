package trust

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActivateProtectedPayloadAndOfflineExpiry(t *testing.T) {
	root, g, clock := payloadFixture(t)
	p, receipt, err := Activate(t.Context(), root, g.Name, clock)
	if err != nil {
		t.Fatalf("activate: %s", reasonOf(err))
	}
	if !filepath.IsAbs(p.Host) || !strings.HasPrefix(p.Host, filepath.Join(root, "payload", g.Name)+string(filepath.Separator)) || p.Generation != g.Name {
		t.Fatal("payload not generation confined")
	}
	if err := VerifyPayloadIdentity(p, receipt); err != nil {
		t.Fatal("fresh launch identity refused")
	}
	clock.Now = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, _, err := Activate(t.Context(), root, g.Name, clock); err != nil {
		t.Fatalf("expired offline activation: %s", reasonOf(err))
	}
	other, never, _ := payloadFixture(t)
	p, _, err = Activate(t.Context(), other, never.Name, clock)
	if err != ReasonExpired || p.Host != "" {
		t.Fatal("expired never-activated generation produced payload")
	}
}

func TestActivateHostIgnoresPATH(t *testing.T) {
	root, g, clock := payloadFixture(t)
	wantHost := filepath.Join(root, "payload", g.Name, filepath.FromSlash(g.Host))
	signedHost, err := os.ReadFile(wantHost)
	if err != nil {
		t.Fatal("read signed host fixture")
	}
	pathDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("resolve PATH fixture directory")
	}
	if err := os.WriteFile(filepath.Join(pathDir, filepath.Base(wantHost)), signedHost, 0700); err != nil {
		t.Fatal("copy PATH host fixture")
	}
	t.Setenv("PATH", pathDir)
	got, receipt, err := Activate(t.Context(), root, g.Name, clock)
	if err != nil {
		t.Fatalf("activate with PATH duplicate: %s", reasonOf(err))
	}
	if got.Host != wantHost {
		t.Fatal("activation selected host outside its generation")
	}
	if receipt.Host != wantHost || receipt.Generation != g.Name {
		t.Fatal("activation receipt does not bind generation host")
	}
}

func TestActivateRecheckRefusesTamperBeforePayload(t *testing.T) {
	for _, kind := range []string{"byte", "extra", "mode", "top-level", "directory-mode"} {
		t.Run(kind, func(t *testing.T) {
			root, g, clock := payloadFixture(t)
			dir := filepath.Join(root, "payload", g.Name)
			host := filepath.Join(dir, filepath.FromSlash(g.Host))
			var err error
			switch kind {
			case "byte":
				err = os.WriteFile(host, []byte("evil"), 0755)
			case "extra":
				err = os.WriteFile(filepath.Join(filepath.Dir(host), "extra"), nil, 0600)
			case "mode":
				err = os.Chmod(host, 0644)
			case "top-level":
				if os.Chmod(dir, 0700) != nil {
					t.Fatal("fixture chmod")
				}
				err = os.WriteFile(filepath.Join(dir, "extra"), nil, 0600)
				if os.Chmod(dir, 0500) != nil {
					t.Fatal("fixture chmod")
				}
			case "directory-mode":
				err = os.Chmod(dir, 0700)
			}
			if err != nil {
				t.Fatal("fixture mutation failed")
			}
			p, _, err := Activate(t.Context(), root, g.Name, clock)
			if err == nil || p.Host != "" {
				t.Fatal("tamper yielded payload")
			}
			j, err := loadHandoff(root)
			if err != nil || j.Generations[g.Name].State != "staged" {
				t.Fatal("failed recheck advanced handoff")
			}
		})
	}
}

func TestVerifyPayloadIdentityRefusesReplacementAndSymlink(t *testing.T) {
	for _, kind := range []string{"replacement", "symlink", "content", "args", "host"} {
		t.Run(kind, func(t *testing.T) {
			root, g, clock := payloadFixture(t)
			p, r, err := Activate(t.Context(), root, g.Name, clock)
			if err != nil {
				t.Fatal("fixture activation failed")
			}
			switch kind {
			case "replacement":
				tmp := p.Host + ".replacement"
				if os.WriteFile(tmp, []byte("data"), 0755) != nil || os.Rename(tmp, p.Host) != nil {
					t.Fatal("fixture replacement failed")
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "foreign")
				if os.WriteFile(target, []byte("data"), 0755) != nil || os.Remove(p.Host) != nil || os.Symlink(target, p.Host) != nil {
					t.Fatal("fixture symlink failed")
				}
			case "content":
				if os.WriteFile(p.Host, []byte("evil"), 0755) != nil {
					t.Fatal("fixture content mutation failed")
				}
			case "args":
				p.Args = []string{"changed"}
			case "host":
				p.Host = "service_host"
			}
			if err := VerifyPayloadIdentity(p, r); err != ReasonPayloadIdentity {
				t.Fatal("changed invocation passed launch check")
			}
		})
	}
}

func TestActivateHandoffFenceAndUntrustedClock(t *testing.T) {
	root, g, clock := payloadFixture(t)
	lock := filepath.Join(root, "payload", ".handoff.lock")
	if os.WriteFile(lock, nil, 0600) != nil {
		t.Fatal("fixture lock")
	}
	if p, _, err := Activate(t.Context(), root, g.Name, clock); err == nil || p.Host != "" {
		t.Fatal("writer fence ignored")
	}
	if os.Remove(lock) != nil {
		t.Fatal("fixture unlock")
	}
	clock.Trusted = false
	if p, _, err := Activate(t.Context(), root, g.Name, clock); err != ReasonUnknownClock || p.Host != "" {
		t.Fatal("unknown acquisition clock accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if p, _, err := Activate(ctx, root, g.Name, clock); err == nil || p.Host != "" {
		t.Fatal("cancelled activation produced payload")
	}
}

func TestActivateUsesCurrentRevocationAndReleaseAuthority(t *testing.T) {
	for _, change := range []string{"new_release", "revocation"} {
		t.Run(change, func(t *testing.T) {
			root, g, clock := payloadFixture(t)
			state, err := NewFileState(filepath.Join(root, "trust"))
			if err != nil {
				t.Fatal("fixture authority state")
			}
			v := NewVerifier(Options{Roots: g.Roots, State: state, Clock: clock, OS: g.OS, Arch: g.Arch})
			want := ReasonReplay
			if change == "new_release" {
				set, err := ParseComponentSet(g.Document)
				if err != nil {
					t.Fatal("fixture manifest")
				}
				set.ReleaseSequence++
				key, _ := verifierFixtureKey(t, "release-1")
				doc, envelope := verifierSign(t, set, "component_set", key)
				if out := v.AcceptForAcquisition(doc, envelope); !out.Accepted {
					t.Fatal("fixture newer acquisition")
				}
			} else {
				rootKey, _ := verifierFixtureKey(t, "root-a")
				_, release := verifierFixtureKey(t, "release-1")
				verifierDelegate(t, v, rootKey, release, func(d *Delegation) { d.DelegationSequence = 2; d.RevokedKeyIDs = []string{release.KeyID} })
				want = ReasonRevokedKey
			}
			if p, _, err := Activate(t.Context(), root, g.Name, clock); err != want || p.Host != "" {
				t.Fatalf("stale generation authority accepted or wrong reason: %s", reasonOf(err))
			}
		})
	}
}
