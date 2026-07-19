package dashboard

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultPortDeterministicAndInRange(t *testing.T) {
	ids := []string{"agt_1", "agt_2", "agt_dash", "scott", "agt_0123456789abcdef"}
	seen := map[string]int{}
	for _, id := range ids {
		port := DefaultPort(id)
		if port < 50000 || port > 59999 {
			t.Fatalf("DefaultPort(%q) = %d, want [50000, 59999]", id, port)
		}
		if again := DefaultPort(id); again != port {
			t.Fatalf("DefaultPort(%q) not deterministic: %d then %d", id, port, again)
		}
		seen[id] = port
	}
	if seen["agt_1"] == seen["agt_2"] && seen["agt_1"] == seen["agt_dash"] {
		t.Fatal("DefaultPort collapsed every id onto one port")
	}
}

func TestRegistryPathRejectsUnsafeAgentIDs(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	for _, id := range []string{"", ".", "..", "../evil", "a/b", "agt$", "-lead", string(make([]byte, 200))} {
		if _, err := RegistryPath(id); err == nil {
			t.Errorf("RegistryPath(%q) accepted an unsafe id", id)
		}
	}
	if _, err := RegistryPath("agt_Dash-1.x"); err != nil {
		t.Fatalf("RegistryPath rejected a valid id: %v", err)
	}
}

func TestRegistryWriteReadRemove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	entry := RegistryEntry{
		AgentID:   "agt_dash",
		AgentName: "dash",
		Account:   "default",
		Realm:     "default",
		Port:      51234,
		PID:       os.Getpid(),
		URL:       "http://127.0.0.1:51234/",
		AccessURL: "http://127.0.0.1:51234/?token=00112233445566778899aabbccddeeff",
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := WriteRegistryEntry(entry); err != nil {
		t.Fatalf("write: %v", err)
	}

	path := filepath.Join(home, "dashboards", "agt_dash.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("registry file mode = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("registry dir mode = %o, want 700", perm)
	}

	got, err := ReadRegistryEntry("agt_dash")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.SchemaVersion != RegistrySchemaVersion {
		t.Fatalf("schema_version = %q, want %q", got.SchemaVersion, RegistrySchemaVersion)
	}
	if got.AgentID != entry.AgentID || got.Port != entry.Port || got.PID != entry.PID || got.URL != entry.URL {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.AccessURL != entry.AccessURL {
		t.Fatalf("access_url round trip: got %q, want %q", got.AccessURL, entry.AccessURL)
	}

	if err := RemoveRegistryEntry("agt_dash"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("registry file still present after remove: %v", err)
	}
	if err := RemoveRegistryEntry("agt_dash"); err != nil {
		t.Fatalf("second remove must be idempotent: %v", err)
	}
}

// loopbackPort extracts the numeric port an httptest server bound.
func loopbackPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	addr, ok := srv.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr %T is not *net.TCPAddr", srv.Listener.Addr())
	}
	return addr.Port
}

// closedLoopbackPort binds and immediately releases a loopback port, giving
// back a port number with (almost certainly) nothing listening on it.
func closedLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func TestLiveRegistryEntryDetectsStalePIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)

	// A fake dashboard: any handler carrying the marker header.
	fakeDashboard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(markerHeader, RegistrySchemaVersion)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer fakeDashboard.Close()
	dashboardPort := loopbackPort(t, fakeDashboard)

	if _, live, err := LiveRegistryEntry("agt_dash"); err != nil || live {
		t.Fatalf("missing entry: live=%v err=%v, want false nil", live, err)
	}

	alive := RegistryEntry{AgentID: "agt_dash", PID: os.Getpid(), Port: dashboardPort}
	if err := WriteRegistryEntry(alive); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, live, err := LiveRegistryEntry("agt_dash"); err != nil || !live {
		t.Fatalf("running pid + responding dashboard: live=%v err=%v, want true nil", live, err)
	}

	// PID reuse after a crash: the recorded pid is alive (it is ours) but no
	// dashboard answers on the recorded port, so the entry is stale and a
	// new serve may claim the slot instead of refusing forever.
	reused := RegistryEntry{AgentID: "agt_dash", PID: os.Getpid(), Port: closedLoopbackPort(t)}
	if err := WriteRegistryEntry(reused); err != nil {
		t.Fatalf("write reused: %v", err)
	}
	if _, live, err := LiveRegistryEntry("agt_dash"); err != nil || live {
		t.Fatalf("reused pid, dead port: live=%v err=%v, want false nil", live, err)
	}

	// A foreign service on the recorded port (no marker header) is not a
	// dashboard either.
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer foreign.Close()
	occupied := RegistryEntry{AgentID: "agt_dash", PID: os.Getpid(), Port: loopbackPort(t, foreign)}
	if err := WriteRegistryEntry(occupied); err != nil {
		t.Fatalf("write occupied: %v", err)
	}
	if _, live, err := LiveRegistryEntry("agt_dash"); err != nil || live {
		t.Fatalf("foreign service on port: live=%v err=%v, want false nil", live, err)
	}

	// A pid far beyond any real pid table: signal 0 fails with ESRCH, so the
	// entry is stale even though a dashboard answers on the port.
	stale := RegistryEntry{AgentID: "agt_dash", PID: 1 << 26, Port: dashboardPort}
	if err := WriteRegistryEntry(stale); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	if _, live, err := LiveRegistryEntry("agt_dash"); err != nil || live {
		t.Fatalf("dead pid: live=%v err=%v, want false nil", live, err)
	}

	path := filepath.Join(home, "dashboards", "agt_dash.json")
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	if _, live, err := LiveRegistryEntry("agt_dash"); err != nil || live {
		t.Fatalf("corrupt entry: live=%v err=%v, want false nil", live, err)
	}
}

// markerResponder serves the dashboard marker header so EntryLive treats the
// entry pointing at it as a live dashboard.
func markerResponder(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(markerHeader, RegistrySchemaVersion)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return loopbackPort(t, srv)
}

func TestClaimRegistryEntry(t *testing.T) {
	t.Run("fresh slot is claimed", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		entry := RegistryEntry{AgentID: "agt_dash", PID: os.Getpid(), Port: markerResponder(t)}
		if _, claimed, err := ClaimRegistryEntry(entry); err != nil || !claimed {
			t.Fatalf("claim empty slot: claimed=%v err=%v, want true nil", claimed, err)
		}
		if got, err := ReadRegistryEntry("agt_dash"); err != nil || got.Port != entry.Port {
			t.Fatalf("claimed entry not durable: %+v err=%v", got, err)
		}
	})

	t.Run("live survivor on another port refuses the claim", func(t *testing.T) {
		t.Setenv("WITSELF_HOME", t.TempDir())
		survivor := RegistryEntry{AgentID: "agt_dash", PID: os.Getpid(), Port: markerResponder(t)}
		if err := WriteRegistryEntry(survivor); err != nil {
			t.Fatalf("write survivor: %v", err)
		}
		got, claimed, err := ClaimRegistryEntry(RegistryEntry{AgentID: "agt_dash", PID: os.Getpid(), Port: closedLoopbackPort(t)})
		if err != nil || claimed {
			t.Fatalf("claim against live survivor: claimed=%v err=%v, want false nil", claimed, err)
		}
		if got.Port != survivor.Port {
			t.Fatalf("survivor = %+v, want port %d", got, survivor.Port)
		}
		if disk, err := ReadRegistryEntry("agt_dash"); err != nil || disk.Port != survivor.Port {
			t.Fatalf("refused claim disturbed the slot: %+v err=%v", disk, err)
		}
	})

	t.Run("stale entry recording the claimant's own port is overwritten", func(t *testing.T) {
		// A crash left an entry whose PID was since reused by a running
		// process and whose port this serve has re-bound. No other process
		// can answer on a port the claimant holds, so the slot is stale even
		// though a probe of that port would see the claimant's own marker.
		t.Setenv("WITSELF_HOME", t.TempDir())
		port := markerResponder(t)
		if err := WriteRegistryEntry(RegistryEntry{AgentID: "agt_dash", PID: os.Getpid(), Port: port}); err != nil {
			t.Fatalf("write stale: %v", err)
		}
		if _, claimed, err := ClaimRegistryEntry(RegistryEntry{AgentID: "agt_dash", PID: os.Getpid(), Port: port}); err != nil || !claimed {
			t.Fatalf("claim own-port slot: claimed=%v err=%v, want true nil", claimed, err)
		}
	})

	t.Run("racing claims admit exactly one winner", func(t *testing.T) {
		// Both dashboards are already answering (marker responders) when
		// they claim, mirroring serveDashboard's serve-then-claim order. The
		// flock serializes the check-and-write, so whichever claim runs
		// second sees the winner live and backs off — under any
		// interleaving, unlike a write-then-re-read guard.
		t.Setenv("WITSELF_HOME", t.TempDir())
		entries := []RegistryEntry{
			{AgentID: "agt_dash", PID: os.Getpid(), Port: markerResponder(t)},
			{AgentID: "agt_dash", PID: os.Getpid(), Port: markerResponder(t)},
		}
		results := make(chan bool, len(entries))
		errs := make(chan error, len(entries))
		for _, entry := range entries {
			go func(entry RegistryEntry) {
				_, claimed, err := ClaimRegistryEntry(entry)
				results <- claimed
				errs <- err
			}(entry)
		}
		winners := 0
		for range entries {
			if err := <-errs; err != nil {
				t.Fatalf("claim: %v", err)
			}
			if <-results {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("racing claims produced %d winners, want exactly 1", winners)
		}
	})
}

// TestReleaseRegistryEntryOnlyRemovesOwnEntry proves the shutdown path never
// deletes a registry slot another process has since claimed.
func TestReleaseRegistryEntryOnlyRemovesOwnEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)

	entry := RegistryEntry{AgentID: "agt_dash", PID: 1234, Port: 51234}
	if err := WriteRegistryEntry(entry); err != nil {
		t.Fatalf("write: %v", err)
	}
	path := filepath.Join(home, "dashboards", "agt_dash.json")

	if err := ReleaseRegistryEntry("agt_dash", 5678); err == nil {
		t.Fatal("release by a non-owner pid must report, not silently succeed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("non-owner release removed the entry: %v", err)
	}

	if err := ReleaseRegistryEntry("agt_dash", 1234); err != nil {
		t.Fatalf("owner release: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("entry still present after owner release: %v", err)
	}
	if err := ReleaseRegistryEntry("agt_dash", 1234); err != nil {
		t.Fatalf("second release must be idempotent: %v", err)
	}
}

func TestListRegistryEntriesScansAndSkipsCorrupt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)

	entries, err := ListRegistryEntries()
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing registry dir: entries=%v err=%v, want none nil", entries, err)
	}

	for _, entry := range []RegistryEntry{
		{AgentID: "agt_b", AgentName: "beta", Port: 51001, PID: 1},
		{AgentID: "agt_a", AgentName: "alpha", Port: 51002, PID: 2},
	} {
		if err := WriteRegistryEntry(entry); err != nil {
			t.Fatalf("write %s: %v", entry.AgentID, err)
		}
	}
	dir := filepath.Join(home, "dashboards")
	if err := os.WriteFile(filepath.Join(dir, "agt_bad.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("stray write: %v", err)
	}

	entries, err = ListRegistryEntries()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 || entries[0].AgentName != "alpha" || entries[1].AgentName != "beta" {
		t.Fatalf("entries = %+v, want alpha then beta", entries)
	}
	if entries[0].SchemaVersion != RegistrySchemaVersion {
		t.Fatalf("schema_version = %q, want %q", entries[0].SchemaVersion, RegistrySchemaVersion)
	}
}
