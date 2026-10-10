// Package memlimit configures Go's soft memory limit from the container and
// samples memory without making startup or telemetry depend on cgroup support.
package memlimit

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

// Result describes the cgroup found at startup. SoftLimit is zero unless this
// package applied a limit. CgroupDir is an absolute path within the supplied root.
type Result struct {
	Source      string
	CgroupLimit int64
	SoftLimit   int64
	CgroupDir   string
}

var current struct {
	sync.RWMutex
	result Result
}

// Configure applies the container policy before the process starts serving.
func Configure(w io.Writer, process string) Result {
	result := ConfigureFrom("/", os.LookupEnv, debug.SetMemoryLimit, w, process)
	current.Lock()
	current.result = result
	current.Unlock()
	return result
}

// Current returns the last result published by Configure.
func Current() Result {
	current.RLock()
	defer current.RUnlock()
	return current.result
}

// ConfigureFrom is Configure's filesystem and runtime seam. Environment values
// are never logged; even a present empty value is an operator override.
func ConfigureFrom(root string, lookupEnv func(string) (string, bool), set func(int64) int64, w io.Writer, process string) Result {
	if w == nil {
		w = io.Discard
	}
	result, reason := detect(root)
	if _, present := lookupEnv("GOMEMLIMIT"); present {
		softLimit := set(-1)
		_, _ = fmt.Fprintf(w, "%s: memory limit unchanged source=\"env\" cgroup_source=%q cgroup_limit=%d soft_limit=%d\n", process, result.Source, result.CgroupLimit, softLimit)
		return result
	}
	if result.CgroupLimit > 0 {
		// Divide first: every finite limit fits, including values near 2^62.
		result.SoftLimit = result.CgroupLimit / 4 * 3
		set(result.SoftLimit)
		_, _ = fmt.Fprintf(w, "%s: memory limit applied source=%q cgroup_limit=%d soft_limit=%d factor_percent=75\n", process, result.Source, result.CgroupLimit, result.SoftLimit)
		return result
	}
	_, _ = fmt.Fprintf(w, "%s: memory limit unchanged source=\"none\" reason=%q\n", process, reason)
	return result
}

func rooted(root, path string) string {
	return filepath.Join(root, strings.TrimPrefix(filepath.Clean("/"+path), "/"))
}

func detect(root string) (Result, string) {
	type candidate struct{ source, dir, file string }
	v2 := []candidate{{"cgroup-v2", "/sys/fs/cgroup", "memory.max"}}
	v1 := []candidate{{"cgroup-v1", "/sys/fs/cgroup/memory", "memory.limit_in_bytes"}}
	if raw, err := os.ReadFile(rooted(root, "/proc/self/cgroup")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			parts := strings.SplitN(line, ":", 3)
			if len(parts) != 3 {
				continue
			}
			if parts[0] == "0" && parts[1] == "" {
				v2 = append(v2, candidate{"cgroup-v2", filepath.Join("/sys/fs/cgroup", parts[2]), "memory.max"})
			}
			for _, controller := range strings.Split(parts[1], ",") {
				if controller == "memory" {
					v1 = append(v1, candidate{"cgroup-v1", filepath.Join("/sys/fs/cgroup/memory", parts[2]), "memory.limit_in_bytes"})
				}
			}
		}
	}
	var finite, firstNonFinite Result
	var firstReason string
	rememberNonFinite := func(result Result, reason string) {
		if firstReason == "" {
			firstNonFinite, firstReason = result, reason
		}
	}
	for _, c := range append(v2, v1...) {
		raw, err := os.ReadFile(rooted(root, filepath.Join(c.dir, c.file)))
		if os.IsNotExist(err) {
			continue
		}
		result := Result{Source: c.source, CgroupDir: c.dir}
		if err != nil {
			rememberNonFinite(result, "unreadable")
			continue
		}
		value := strings.TrimSpace(string(raw))
		if value == "max" {
			rememberNonFinite(result, "unlimited")
			continue
		}
		// ParseUint accepts a leading plus; the cgroup format accepts digits only.
		if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			rememberNonFinite(result, "unparseable")
			continue
		}
		limit, err := strconv.ParseUint(value, 10, 64)
		if err != nil || limit == 0 {
			rememberNonFinite(result, "unparseable")
			continue
		}
		if limit >= 1<<62 {
			rememberNonFinite(result, "unlimited")
			continue
		}
		result.CgroupLimit = int64(limit)
		if finite.CgroupLimit == 0 || result.CgroupLimit < finite.CgroupLimit {
			finite = result
		}
	}
	if finite.CgroupLimit > 0 {
		return finite, ""
	}
	if firstReason != "" {
		return firstNonFinite, firstReason
	}
	return Result{Source: "none"}, "no_cgroup_file"
}
