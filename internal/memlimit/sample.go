package memlimit

import (
	"os"
	"path/filepath"
	"runtime/metrics"
	"strconv"
	"strings"
)

// Snapshot holds bytes (except GCCycles). An unavailable value is -1.
type Snapshot struct {
	GoLive, GoGoal, GoTotal, GoLimit, GCCycles int64
	Anon, File, MemoryPeak, RSSHWM, RSS        int64
}

// Sample samples this process and the cgroup discovered by Configure.
func Sample() Snapshot { return SampleFrom("/", Current().CgroupDir) }

// SampleFrom reads all Go metrics together, then optional kernel counters.
// cgroupDir is an absolute path within root, as returned by ConfigureFrom.
func SampleFrom(root, cgroupDir string) Snapshot {
	s := Snapshot{GoLive: -1, GoGoal: -1, GoTotal: -1, GoLimit: -1, GCCycles: -1, Anon: -1, File: -1, MemoryPeak: -1, RSSHWM: -1, RSS: -1}
	values := []metrics.Sample{
		{Name: "/gc/heap/live:bytes"},
		{Name: "/gc/heap/goal:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
		{Name: "/gc/gomemlimit:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	metrics.Read(values)
	read := func(i int) int64 {
		if values[i].Value.Kind() != metrics.KindUint64 || values[i].Value.Uint64() > 1<<63-1 {
			return -1
		}
		return int64(values[i].Value.Uint64())
	}
	s.GoLive, s.GoGoal, s.GoLimit, s.GCCycles = read(0), read(1), read(4), read(5)
	if total, released := read(2), read(3); total >= 0 && released >= 0 && total >= released {
		s.GoTotal = total - released
	}
	if cgroupDir != "" {
		if raw, err := os.ReadFile(rooted(root, filepath.Join(cgroupDir, "memory.stat"))); err == nil {
			fields := parseFields(string(raw))
			s.Anon = first(fields, "anon", "total_rss", "rss")
			s.File = first(fields, "file", "total_cache", "cache")
		}
		if raw, err := os.ReadFile(rooted(root, filepath.Join(cgroupDir, "memory.peak"))); err == nil {
			s.MemoryPeak = integer(strings.TrimSpace(string(raw)))
		}
	}
	if raw, err := os.ReadFile(rooted(root, "/proc/self/status")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[2] != "kB" {
				continue
			}
			value := integer(fields[1])
			if value < 0 || value > (1<<63-1)/1024 {
				continue
			}
			switch fields[0] {
			case "VmHWM:":
				s.RSSHWM = value * 1024
			case "VmRSS:":
				s.RSS = value * 1024
			}
		}
	}
	return s
}

func integer(value string) int64 {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

func parseFields(raw string) map[string]int64 {
	result := make(map[string]int64)
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			result[fields[0]] = integer(fields[1])
		}
	}
	return result
}

func first(fields map[string]int64, keys ...string) int64 {
	for _, key := range keys {
		if value, ok := fields[key]; ok && value >= 0 {
			return value
		}
	}
	return -1
}
