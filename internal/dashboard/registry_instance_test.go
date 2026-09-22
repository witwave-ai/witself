package dashboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func registryInstanceFixture() RegistryEntry {
	return RegistryEntry{SchemaVersion: RegistrySchemaVersion, AgentID: "agt_exact", PID: 12345, Port: 51234, URL: "http://127.0.0.1:51234/", AccessURL: "http://127.0.0.1:51234/?token=" + strings.Repeat("ab", 16), StartedAt: time.Now().UTC(), AccountID: "acc_synthetic", RealmID: "rlm_synthetic", Endpoint: "https://synthetic.invalid"}
}

func TestReleaseRegistryInstanceExactFence(t *testing.T) {
	cases := map[string]func(*RegistryEntry){
		"pid":                    func(e *RegistryEntry) { e.PID++ },
		"port same pid":          func(e *RegistryEntry) { e.Port++ },
		"started same pid":       func(e *RegistryEntry) { e.StartedAt = e.StartedAt.Add(time.Nanosecond) },
		"private token same pid": func(e *RegistryEntry) { e.AccessURL += "x" },
		"agent":                  func(e *RegistryEntry) { e.AgentID = "agt_other" },
		"account":                func(e *RegistryEntry) { e.AccountID = "acc_other" },
		"realm":                  func(e *RegistryEntry) { e.RealmID = "rlm_other" },
		"endpoint":               func(e *RegistryEntry) { e.Endpoint = "https://other.invalid" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", t.TempDir())
			original := registryInstanceFixture()
			if err := WriteRegistryEntry(original); err != nil {
				t.Fatal(err)
			}
			successor := original
			change(&successor)
			// Write to the selected filename even for a malicious in-record agent ID.
			path, err := RegistryPath(original.AgentID)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeJSONAtomic(path, successor); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			removed, _ := ReleaseRegistryInstance(context.Background(), original)
			after, err := os.ReadFile(path)
			if removed || err != nil || string(after) != string(before) {
				t.Fatal("successor changed by old owner release")
			}
		})
	}
	t.Run("own entry and missing entry", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		entry := registryInstanceFixture()
		if err := WriteRegistryEntry(entry); err != nil {
			t.Fatal(err)
		}
		if removed, err := ReleaseRegistryInstance(context.Background(), entry); err != nil || !removed {
			t.Fatalf("release: removed=%v error=%v", removed, err)
		}
		if removed, err := ReleaseRegistryInstance(context.Background(), entry); err != nil || removed {
			t.Fatalf("missing release: removed=%v error=%v", removed, err)
		}
	})
}

func TestReleaseRegistryInstancePreservesCorruption(t *testing.T) {
	for _, mode := range []string{"corrupt", "oversized", "symlink", "directory"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", t.TempDir())
			entry := registryInstanceFixture()
			path, err := RegistryPath(entry.AgentID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte("private"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			default:
				raw := "{bad"
				if mode == "oversized" {
					raw = strings.Repeat("x", 16385)
				}
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if removed, err := ReleaseRegistryInstance(context.Background(), entry); err == nil || removed {
				t.Fatalf("untrusted release removed=%v error=%v", removed, err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("untrusted entry was removed")
			}
		})
	}
}

func TestRegistryInstanceReleaseUsesClaimLock(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	entry := registryInstanceFixture()
	if err := WriteRegistryEntry(entry); err != nil {
		t.Fatal(err)
	}
	path, err := RegistryPath(entry.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockRegistryClaim(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	// A claim owns the lock while replacing a stale instance. A release cannot
	// read the old record now and unlink the successor after this claim exits.
	done := make(chan bool, 1)
	go func() { removed, _ := ReleaseRegistryInstance(context.Background(), entry); done <- removed }()
	select {
	case <-done:
		unlock()
		t.Fatal("release bypassed claim lock")
	case <-time.After(50 * time.Millisecond):
	}
	successor := entry
	successor.StartedAt = entry.StartedAt.Add(time.Second)
	if err := WriteRegistryEntry(successor); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	select {
	case removed := <-done:
		if removed {
			t.Fatal("release removed racing successor")
		}
	case <-time.After(time.Second):
		t.Fatal("release remained blocked")
	}
	current, err := ReadRegistryInstance(entry.AgentID)
	if err != nil || !SameRegistryInstance(current, successor) {
		t.Fatal("claim successor was not preserved")
	}
}

func TestRegistryInstanceLockCancellation(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	entry := registryInstanceFixture()
	if err := WriteRegistryEntry(entry); err != nil {
		t.Fatal(err)
	}
	path, err := RegistryPath(entry.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockRegistryClaim(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	called := false
	if matched, err := WithRegistryInstance(ctx, entry, func() error { called = true; return nil }); !errors.Is(err, context.DeadlineExceeded) || matched || called {
		t.Fatalf("canceled lock: matched=%v called=%v error=%v", matched, called, err)
	}
}

func TestClaimManagedRegistryEntryRefusesUnverifiedRecords(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	entry := registryInstanceFixture()
	if claimed, err := ClaimManagedRegistryEntry(context.Background(), entry); err != nil || !claimed {
		t.Fatalf("initial claim=%v error=%v", claimed, err)
	}
	original, err := ReadRegistryInstance(entry.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	candidate := entry
	candidate.Port++
	if claimed, err := ClaimManagedRegistryEntry(context.Background(), candidate); err != nil || claimed {
		t.Fatalf("occupied claim=%v error=%v", claimed, err)
	}
	got, err := ReadRegistryInstance(entry.AgentID)
	if err != nil || !SameRegistryInstance(got, original) {
		t.Fatal("managed claim replaced record")
	}
	path, err := RegistryPath(entry.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if claimed, err := ClaimManagedRegistryEntry(context.Background(), candidate); err != nil || claimed {
		t.Fatalf("corrupt claim=%v error=%v", claimed, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "{corrupt" {
		t.Fatal("managed claim changed corrupt record")
	}
}
