package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
)

func TestMemoryVectorCLIProfileAndSetVerticalSlice(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("agent-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vectorFile := filepath.Join(t.TempDir(), "vector.json")
	if err := os.WriteFile(vectorFile, []byte(`[12345.6789,-98765.4321]`), 0o600); err != nil {
		t.Fatal(err)
	}
	contentHash := strings.Repeat("b", 64)
	vectorHash := strings.Repeat("c", 64)
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Errorf("authorization = %q", got)
		}
		key := r.Method + " " + r.URL.Path
		mu.Lock()
		seen[key]++
		mu.Unlock()
		switch key {
		case "POST /v1/memory-vector-profiles":
			var in client.CreateMemoryVectorProfileInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode profile: %v", err)
			}
			if in.Provider != "local" || in.Model != "embed-v1" || in.Recipe != "plain" ||
				in.RecipeVersion != "1" || in.Dimensions != 2 || in.DistanceMetric != "cosine" || in.Normalization != "l2" {
				t.Errorf("profile input = %#v", in)
			}
			_ = json.NewEncoder(w).Encode(client.MemoryVectorProfile{
				ID: "mvp_1", Provider: in.Provider, Model: in.Model, Recipe: in.Recipe,
				RecipeVersion: in.RecipeVersion, Dimensions: in.Dimensions,
				DistanceMetric: in.DistanceMetric, Normalization: in.Normalization,
				ContractHash: strings.Repeat("a", 64),
			})
		case "GET /v1/memory-vector-profiles":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []client.MemoryVectorProfile{{
				ID: "mvp_1", Provider: "local", Model: "embed-v1", Recipe: "plain",
				RecipeVersion: "1", Dimensions: 2, DistanceMetric: "cosine",
				Normalization: "l2", ContractHash: strings.Repeat("a", 64),
			}}})
		case "POST /v1/memory-vectors":
			var in client.PutMemoryVectorInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode vector: %v", err)
			}
			if in.ProfileID != "mvp_1" || in.MemoryID != "mem_1" || in.MemoryVersion != 3 ||
				in.ContentHash != contentHash || len(in.Vector) != 2 ||
				in.Vector[0] != 12345.6789 || in.Vector[1] != -98765.4321 {
				t.Errorf("vector input = %#v", in)
			}
			_ = json.NewEncoder(w).Encode(client.MemoryVectorReceipt{
				ProfileID: in.ProfileID, MemoryID: in.MemoryID, MemoryVersion: in.MemoryVersion,
				ContentHash: in.ContentHash, VectorHash: vectorHash, Dimensions: len(in.Vector),
			})
		default:
			t.Errorf("unexpected request %s", key)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	connection := []string{"--endpoint", srv.URL, "--token-file", tokenFile}
	commands := []struct {
		args []string
		want string
	}{
		{append([]string{"memory", "vector", "profile", "create"}, append(connection,
			"--provider", "local", "--model", "embed-v1", "--recipe", "plain",
			"--recipe-version", "1", "--dimensions", "2", "--metric", "cosine", "--normalization", "l2")...), "mvp_1"},
		{append([]string{"memory", "vector", "profile", "list"}, connection...), "embed-v1"},
		{append([]string{"memory", "vector", "set", "mem_1"}, append(connection,
			"--profile", "mvp_1", "--memory-version", "3", "--content-hash", contentHash,
			"--vector-file", vectorFile, "--json")...), vectorHash},
	}
	for _, command := range commands {
		stdout, stderr, code := captureFactDeleteCLI(t, func() int { return run(command.args) })
		if code != 0 {
			t.Fatalf("run(%q) = %d\nstdout: %s\nstderr: %s", command.args, code, stdout, stderr)
		}
		if !strings.Contains(stdout, command.want) {
			t.Errorf("run(%q) output missing %q: %s", command.args, command.want, stdout)
		}
		combined := stdout + stderr
		if strings.Contains(combined, "12345.6789") || strings.Contains(combined, "-98765.4321") ||
			strings.Contains(combined, `"vector":`) {
			t.Errorf("run(%q) leaked raw vector: %s", command.args, combined)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen["POST /v1/memory-vector-profiles"] != 1 ||
		seen["GET /v1/memory-vector-profiles"] != 1 || seen["POST /v1/memory-vectors"] != 1 {
		t.Fatalf("requests = %#v", seen)
	}
}

func TestReadMemoryVectorFileIsBoundedAndStrict(t *testing.T) {
	dir := t.TempDir()
	validPath := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(validPath, []byte(`[1.25,-2,3e-4]`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readMemoryVectorFile(validPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 1.25 || got[1] != -2 || got[2] != 0.0003 {
		t.Fatalf("vector = %#v", got)
	}

	trailingPath := filepath.Join(dir, "trailing.json")
	if err := os.WriteFile(trailingPath, []byte(`[1,2] {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMemoryVectorFile(trailingPath); err == nil || !strings.Contains(err.Error(), "expected one JSON array") {
		t.Fatalf("trailing JSON error = %v", err)
	}

	oversizedPath := filepath.Join(dir, "oversized.json")
	oversized := strings.Repeat(" ", maxMemoryVectorFileBytes+1)
	if err := os.WriteFile(oversizedPath, []byte(oversized), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMemoryVectorFile(oversizedPath); err == nil ||
		!strings.Contains(err.Error(), "input exceeds 262144 bytes") {
		t.Fatalf("oversized error = %v", err)
	}

	if vector, err := readMemoryVectorFile(""); err != nil || vector != nil {
		t.Fatalf("empty path = %#v / %v", vector, err)
	}
}

func TestMemoryVectorCLIRejectsOversizedFileBeforeNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(path, []byte(strings.Repeat("0", maxMemoryVectorFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := captureFactDeleteCLI(t, func() int {
		return run([]string{
			"memory", "vector", "set", "mem_1", "--profile", "mvp_1",
			"--memory-version", "1", "--content-hash", strings.Repeat("a", 64),
			"--vector-file", path,
		})
	})
	if code != 2 || stdout != "" || !strings.Contains(stderr, "input exceeds 262144 bytes") {
		t.Fatalf("oversized CLI = code:%d stdout:%q stderr:%q", code, stdout, stderr)
	}
}

func TestMemoryVectorCLIRejectsMissingGuards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vector.json")
	if err := os.WriteFile(path, []byte(`[1,2]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{
		{"memory", "vector", "profile", "create", "--provider", "local", "--model", "embed-v1", "--recipe", "plain", "--dimensions", "2"},
		{"memory", "vector", "set", "mem_1", "--profile", "mvp_1", "--memory-version", "1", "--vector-file", path},
	} {
		_, _, code := captureFactDeleteCLI(t, func() int { return run(command) })
		if code != 2 {
			t.Errorf("run(%q) = %d, want 2", command, code)
		}
	}
}
