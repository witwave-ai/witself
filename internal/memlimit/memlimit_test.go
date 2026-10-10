package memlimit

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixture(t *testing.T, root, path, contents string) {
	t.Helper()
	name := rooted(root, path)
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureFrom(t *testing.T) {
	for _, tc := range []struct {
		name, file, value, cgroup, source, reason string
		limit, soft                               int64
		directory                                 bool
	}{
		{name: "v2", file: "/sys/fs/cgroup/memory.max", value: "268435456\n", source: "cgroup-v2", limit: 268435456, soft: 201326592},
		{name: "v2 unlimited", file: "/sys/fs/cgroup/memory.max", value: "max", reason: "unlimited"},
		{name: "v2 nested", file: "/sys/fs/cgroup/kubepods/p/c/memory.max", value: "536870912", cgroup: "0::/kubepods/p/c\n", source: "cgroup-v2", limit: 536870912, soft: 402653184},
		{name: "v1", file: "/sys/fs/cgroup/memory/memory.limit_in_bytes", value: "268435456", source: "cgroup-v1", limit: 268435456, soft: 201326592},
		{name: "v1 nested", file: "/sys/fs/cgroup/memory/nested/memory.limit_in_bytes", value: "268435456", cgroup: "3:cpu,memory:/nested\n", source: "cgroup-v1", limit: 268435456, soft: 201326592},
		{name: "v1 unlimited", file: "/sys/fs/cgroup/memory/memory.limit_in_bytes", value: "9223372036854771712", reason: "unlimited"},
		{name: "missing", reason: "no_cgroup_file"},
		{name: "letters", file: "/sys/fs/cgroup/memory.max", value: "abc", reason: "unparseable"},
		{name: "empty", file: "/sys/fs/cgroup/memory.max", reason: "unparseable"},
		{name: "zero", file: "/sys/fs/cgroup/memory.max", value: "0", reason: "unparseable"},
		{name: "negative", file: "/sys/fs/cgroup/memory.max", value: "-1", reason: "unparseable"},
		{name: "overflow", file: "/sys/fs/cgroup/memory.max", value: "18446744073709551616", reason: "unparseable"},
		{name: "directory", file: "/sys/fs/cgroup/memory.max", directory: true, reason: "unreadable"},
		{name: "finite near threshold", file: "/sys/fs/cgroup/memory.max", value: "4611686018427387903", source: "cgroup-v2", limit: 4611686018427387903, soft: 3458764513820540925},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.directory {
				if err := os.MkdirAll(rooted(root, tc.file), 0o700); err != nil {
					t.Fatal(err)
				}
			} else if tc.file != "" {
				fixture(t, root, tc.file, tc.value)
			}
			if tc.cgroup != "" {
				fixture(t, root, "/proc/self/cgroup", tc.cgroup)
			}
			var calls []int64
			var output bytes.Buffer
			result := ConfigureFrom(root, func(string) (string, bool) { return "", false }, func(v int64) int64 {
				calls = append(calls, v)
				return 99
			}, &output, "test")
			if strings.Count(output.String(), "\n") != 1 {
				t.Fatalf("want one line: %q", output.String())
			}
			if result.CgroupLimit != tc.limit || result.SoftLimit != tc.soft {
				t.Fatalf("result = %+v, want limit=%d soft=%d", result, tc.limit, tc.soft)
			}
			if tc.soft > 0 {
				if !reflect.DeepEqual(calls, []int64{tc.soft}) || result.Source != tc.source || result.CgroupDir != filepath.Dir(tc.file) {
					t.Fatalf("calls=%v result=%+v", calls, result)
				}
				if !strings.Contains(output.String(), "source=\""+tc.source+"\"") || !strings.Contains(output.String(), "factor_percent=75") {
					t.Fatalf("unexpected applied line: %q", output.String())
				}
			} else if len(calls) != 0 || !strings.Contains(output.String(), "reason=\""+tc.reason+"\"") {
				t.Fatalf("calls=%v line=%q", calls, output.String())
			}
		})
	}
}

func TestConfigureFromCandidates(t *testing.T) {
	for _, tc := range []struct {
		name, rootValue, leafValue, reason string
		v1, rootDirectory, wantRoot        bool
		limit                              int64
	}{
		{name: "v1 unlimited root with finite nested limit", v1: true, rootValue: "9223372036854771712", leafValue: "268435456", limit: 268435456},
		{name: "v2 unlimited root with finite leaf", rootValue: "max", leafValue: "268435456", limit: 268435456},
		{name: "unreadable root with finite leaf", rootDirectory: true, leafValue: "268435456", limit: 268435456},
		{name: "unparseable root with finite leaf", rootValue: "invalid", leafValue: "268435456", limit: 268435456},
		{name: "smallest finite leaf", rootValue: "536870912", leafValue: "268435456", limit: 268435456},
		{name: "smallest finite root", rootValue: "268435456", leafValue: "536870912", wantRoot: true, limit: 268435456},
		{name: "equal finite limits keep candidate order", rootValue: "268435456", leafValue: "268435456", wantRoot: true, limit: 268435456},
		{name: "finite root with unlimited leaf", rootValue: "268435456", leafValue: "max", wantRoot: true, limit: 268435456},
		{name: "first unlimited reason", rootValue: "max", leafValue: "invalid", wantRoot: true, reason: "unlimited"},
		{name: "first unparseable reason", rootValue: "invalid", leafValue: "max", wantRoot: true, reason: "unparseable"},
		{name: "first unreadable reason", rootDirectory: true, leafValue: "max", wantRoot: true, reason: "unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			source, dir, file, cgroup := "cgroup-v2", "/sys/fs/cgroup", "memory.max", "0::/nested\n"
			if tc.v1 {
				source, dir, file, cgroup = "cgroup-v1", "/sys/fs/cgroup/memory", "memory.limit_in_bytes", "3:cpu,memory:/nested\n"
			}
			if tc.rootDirectory {
				if err := os.MkdirAll(rooted(root, filepath.Join(dir, file)), 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				fixture(t, root, filepath.Join(dir, file), tc.rootValue)
			}
			fixture(t, root, filepath.Join(dir, "nested", file), tc.leafValue)
			fixture(t, root, "/proc/self/cgroup", cgroup)
			var calls []int64
			var output bytes.Buffer
			result := ConfigureFrom(root, func(string) (string, bool) { return "", false }, func(v int64) int64 {
				calls = append(calls, v)
				return 99
			}, &output, "test")
			wantDir := filepath.Join(dir, "nested")
			if tc.wantRoot {
				wantDir = dir
			}
			want := Result{Source: source, CgroupLimit: tc.limit, CgroupDir: wantDir}
			if tc.limit > 0 {
				want.SoftLimit = 201326592
				if !reflect.DeepEqual(calls, []int64{201326592}) {
					t.Fatalf("setter calls = %v, want [201326592]", calls)
				}
				if strings.Contains(output.String(), "reason=") || !strings.Contains(output.String(), "factor_percent=75") {
					t.Fatalf("unexpected applied line: %q", output.String())
				}
			} else if len(calls) != 0 || !strings.Contains(output.String(), "reason=\""+tc.reason+"\"") {
				t.Fatalf("calls=%v line=%q", calls, output.String())
			}
			if result != want {
				t.Fatalf("result = %+v, want %+v", result, want)
			}
			if strings.Count(output.String(), "\n") != 1 {
				t.Fatalf("want one line: %q", output.String())
			}
		})
	}
}

func TestConfigureFromEnvironmentOverride(t *testing.T) {
	for _, value := range []string{"off", "300MiB", ""} {
		t.Run("value_"+value, func(t *testing.T) {
			root := t.TempDir()
			fixture(t, root, "/sys/fs/cgroup/memory.max", "268435456")
			var calls []int64
			var output bytes.Buffer
			result := ConfigureFrom(root, func(key string) (string, bool) {
				if key != "GOMEMLIMIT" {
					t.Fatalf("unexpected environment lookup %q", key)
				}
				return value, true
			}, func(v int64) int64 {
				calls = append(calls, v)
				return 123
			}, &output, "test")
			if len(calls) > 1 || len(calls) == 1 && calls[0] != -1 {
				t.Fatalf("operator override mutated limit: calls=%v", calls)
			}
			if result.CgroupLimit != 268435456 || result.Source != "cgroup-v2" || result.CgroupDir != "/sys/fs/cgroup" || result.SoftLimit != 0 {
				t.Fatalf("override lost cgroup detection: %+v", result)
			}
			want := "test: memory limit unchanged source=\"env\" cgroup_source=\"cgroup-v2\" cgroup_limit=268435456 soft_limit=123\n"
			if output.String() != want {
				t.Fatalf("line=%q, want %q", output.String(), want)
			}
		})
	}
}

func TestSampleFrom(t *testing.T) {
	for _, tc := range []struct {
		name, stat string
		anon, file int64
	}{
		{"v2", "anon 123\nfile 456\n", 123, 456},
		{"v1 totals", "rss 1\ncache 2\ntotal_rss 345\ntotal_cache 678\n", 345, 678},
		{"v1 fallback", "rss 789\ncache 987\n", 789, 987},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fixture(t, root, "/sys/fs/cgroup/memory.stat", tc.stat)
			fixture(t, root, "/sys/fs/cgroup/memory.peak", "999\n")
			fixture(t, root, "/proc/self/status", "Name: test\nVmHWM:  123456 kB\nVmRSS:  2048 kB\n")
			s := SampleFrom(root, "/sys/fs/cgroup")
			if s.Anon != tc.anon || s.File != tc.file || s.MemoryPeak != 999 || s.RSSHWM != 126418944 || s.RSS != 2097152 {
				t.Fatalf("sample=%+v", s)
			}
			if s.GoTotal <= 0 || s.GoLive < 0 || s.GoGoal <= 0 || s.GoLimit <= 0 || s.GCCycles < 0 {
				t.Fatalf("Go metrics unavailable: %+v", s)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		s := SampleFrom(t.TempDir(), "/sys/fs/cgroup")
		if s.Anon != -1 || s.File != -1 || s.MemoryPeak != -1 || s.RSSHWM != -1 || s.RSS != -1 {
			t.Fatalf("missing counters must be unavailable: %+v", s)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		root := t.TempDir()
		fixture(t, root, "/sys/fs/cgroup/memory.stat", "anon invalid\nfile -1\n")
		fixture(t, root, "/sys/fs/cgroup/memory.peak", "-5")
		fixture(t, root, "/proc/self/status", "VmHWM: 9223372036854775807 kB\nVmRSS: 2048 bytes\n")
		s := SampleFrom(root, "/sys/fs/cgroup")
		if s.Anon != -1 || s.File != -1 || s.MemoryPeak != -1 || s.RSSHWM != -1 || s.RSS != -1 {
			t.Fatalf("malformed counters must be unavailable: %+v", s)
		}
	})
}
