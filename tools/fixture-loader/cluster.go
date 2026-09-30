package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type thresholds struct {
	memory, rise, pvc, fs float64
}

type nodeReading struct {
	name               string
	memory, fs         float64
	fsUsed, fsCapacity int64
}

type readings struct {
	nodes                []nodeReading
	pvc                  float64
	pvcUsed, pvcCapacity int64
	restarts             int
	ready                bool
	lastReason           string
}

const nodesJSONPath = `jsonpath={range .items[*]}{.metadata.name}{"\t"}{.status.allocatable.memory}{"\n"}{end}`
const postgresJSONPath = `jsonpath={.spec.nodeName}{"\t"}{.status.containerStatuses[?(@.name=="postgresql")].restartCount}{"\t"}{.status.containerStatuses[?(@.name=="postgresql")].ready}{"\t"}{.status.containerStatuses[?(@.name=="postgresql")].lastState.terminated.reason}`
const ingressJSONPath = `jsonpath={.spec.rules[*].host}`

var nodeNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
var memoryQuantityPattern = regexp.MustCompile(`^([0-9]+)(Ki|Mi|Gi|Ti|k|M|G|T)?$`)

func parseMemoryQuantity(value string) (int64, error) {
	matches := memoryQuantityPattern.FindStringSubmatch(value)
	if matches == nil {
		return 0, errors.New("invalid memory quantity")
	}
	n, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return 0, errors.New("invalid memory quantity")
	}
	multipliers := map[string]int64{"": 1, "Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "k": 1000, "M": 1000000, "G": 1000000000, "T": 1000000000000}
	multiplier := multipliers[matches[2]]
	if n > math.MaxInt64/multiplier {
		return 0, errors.New("invalid memory quantity")
	}
	return n * multiplier, nil
}

type cappedOutput struct {
	bytes.Buffer
}

func (out *cappedOutput) Write(p []byte) (int, error) {
	const limit = 16 << 20
	remaining := limit - out.Len()
	if len(p) > remaining {
		n, _ := out.Buffer.Write(p[:remaining])
		return n, errors.New("subprocess output limit")
	}
	return out.Buffer.Write(p)
}

func execRunner(ctx context.Context, name string, args []string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout cappedOutput
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			code = exitError.ExitCode()
		}
	}
	return stdout.Bytes(), code, err
}

func kubectlArgs(c config, args ...string) []string {
	prefix := make([]string, 0, len(args)+5)
	if c.kubeconfig != "" {
		prefix = append(prefix, "--kubeconfig", c.kubeconfig)
	}
	prefix = append(prefix, "--context", c.kubeContext, "--request-timeout=20s")
	return append(prefix, args...)
}

func quoteArg(arg string) string {
	return "'" + strings.ReplaceAll(safeText(arg), "'", "'\\''") + "'"
}

func kubectlFailure(c config, step string, args []string, code int, during bool) error {
	if during {
		return failure(4, "stopped: kubectl step %s failed (exit %d)", step, code)
	}
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, quoteArg(c.kubectl))
	for _, arg := range args {
		quoted = append(quoted, quoteArg(arg))
	}
	return failure(4, "refused: kubectl step %s failed (exit %d); re-run: %s", step, code, strings.Join(quoted, " "))
}

func kubectlStep(ctx context.Context, c config, d deps, step string, during bool, args ...string) ([]byte, error) {
	argv := kubectlArgs(c, args...)
	stepCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stdout, code, err := d.run(stepCtx, c.kubectl, argv)
	if err != nil || code != 0 || len(stdout) > 16<<20 {
		return nil, kubectlFailure(c, step, argv, code, during)
	}
	return stdout, nil
}

func ingressHosts(ctx context.Context, c config, d deps) ([]string, error) {
	args := []string{"-n", "witself", "get", "ingress", "witself-server", "-o", ingressJSONPath}
	out, err := kubectlStep(ctx, c, d, "K5", false, args...)
	if err != nil {
		return nil, err
	}
	hosts := strings.Fields(string(out))
	if len(hosts) == 0 {
		return nil, kubectlFailure(c, "K5", kubectlArgs(c, args...), 0, false)
	}
	return hosts, nil
}

type volumeStats struct {
	Available *int64 `json:"availableBytes"`
	Capacity  *int64 `json:"capacityBytes"`
}

func (v volumeStats) used() (int64, int64, bool) {
	if v.Available == nil || v.Capacity == nil || *v.Capacity <= 0 || *v.Available < 0 || *v.Available > *v.Capacity {
		return 0, 0, false
	}
	return *v.Capacity - *v.Available, *v.Capacity, true
}

type kubeSummary struct {
	Node struct {
		FS volumeStats `json:"fs"`
	} `json:"node"`
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"podRef"`
		Volumes []struct {
			volumeStats
			PVCRef struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"pvcRef"`
		} `json:"volume"`
	} `json:"pods"`
}

func safeLastReason(reason string) string {
	switch reason {
	case "", "Completed", "Error", "OOMKilled", "ContainerCannotRun", "StartError", "Unknown", "DeadlineExceeded", "Evicted", "Signal":
		return reason
	default:
		return "unknown"
	}
}

func probe(ctx context.Context, c config, d deps, during bool) (readings, error) {
	var result readings
	k1 := []string{"get", "nodes", "-o", nodesJSONPath}
	out, err := kubectlStep(ctx, c, d, "K1", during, k1...)
	if err != nil {
		return result, err
	}
	allocatable := make(map[string]int64)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 2 {
			return result, kubectlFailure(c, "K1", kubectlArgs(c, k1...), 0, during)
		}
		name := fields[0]
		if !nodeNamePattern.MatchString(name) || strings.Contains(name, "..") {
			return result, failure(4, "refused: unsafe node name from kubectl")
		}
		memory, parseErr := parseMemoryQuantity(fields[1])
		if parseErr != nil || memory <= 0 || allocatable[name] != 0 {
			return result, kubectlFailure(c, "K1", kubectlArgs(c, k1...), 0, during)
		}
		allocatable[name] = memory
		result.nodes = append(result.nodes, nodeReading{name: name})
	}
	sort.Slice(result.nodes, func(i, j int) bool { return result.nodes[i].name < result.nodes[j].name })
	k2 := []string{"get", "--raw", "/apis/metrics.k8s.io/v1beta1/nodes"}
	out, err = kubectlStep(ctx, c, d, "K2", during, k2...)
	if err != nil {
		return result, err
	}
	var metrics struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Usage struct {
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &metrics) != nil {
		return result, kubectlFailure(c, "K2", kubectlArgs(c, k2...), 0, during)
	}
	usage := make(map[string]int64)
	for _, metric := range metrics.Items {
		memory, parseErr := parseMemoryQuantity(metric.Usage.Memory)
		_, duplicate := usage[metric.Metadata.Name]
		if parseErr != nil || duplicate || allocatable[metric.Metadata.Name] == 0 {
			return result, kubectlFailure(c, "K2", kubectlArgs(c, k2...), 0, during)
		}
		usage[metric.Metadata.Name] = memory
	}
	if len(usage) != len(allocatable) {
		return result, kubectlFailure(c, "K2", kubectlArgs(c, k2...), 0, during)
	}
	for i := range result.nodes {
		name := result.nodes[i].name
		result.nodes[i].memory = float64(usage[name]) / float64(allocatable[name]) * 100
	}
	k4 := []string{"-n", "witself", "get", "pod", "witself-postgresql-0", "-o", postgresJSONPath}
	out, err = kubectlStep(ctx, c, d, "K4", during, k4...)
	if err != nil {
		return result, err
	}
	postgres := strings.Split(strings.TrimRight(string(out), "\r\n"), "\t")
	if len(postgres) != 4 {
		return result, kubectlFailure(c, "K4", kubectlArgs(c, k4...), 0, during)
	}
	result.restarts, err = strconv.Atoi(postgres[1])
	if err != nil || result.restarts < 0 || allocatable[postgres[0]] == 0 || (postgres[2] != "true" && postgres[2] != "false") {
		return result, kubectlFailure(c, "K4", kubectlArgs(c, k4...), 0, during)
	}
	result.ready = postgres[2] == "true"
	result.lastReason = safeLastReason(postgres[3])
	volumeMatches := 0
	for i := range result.nodes {
		node := &result.nodes[i]
		k3 := []string{"get", "--raw", "/api/v1/nodes/" + node.name + "/proxy/stats/summary"}
		out, err = kubectlStep(ctx, c, d, "K3", during, k3...)
		if err != nil {
			return result, err
		}
		var summary kubeSummary
		if json.Unmarshal(out, &summary) != nil {
			return result, kubectlFailure(c, "K3", kubectlArgs(c, k3...), 0, during)
		}
		used, capacity, valid := summary.Node.FS.used()
		if !valid {
			return result, kubectlFailure(c, "K3", kubectlArgs(c, k3...), 0, during)
		}
		node.fsUsed, node.fsCapacity = used, capacity
		node.fs = float64(used) / float64(capacity) * 100
		if node.name != postgres[0] {
			continue
		}
		for _, pod := range summary.Pods {
			if pod.PodRef.Name != "witself-postgresql-0" || pod.PodRef.Namespace != "witself" {
				continue
			}
			for _, volume := range pod.Volumes {
				if volume.PVCRef.Namespace != "witself" || !strings.Contains(volume.PVCRef.Name, "postgresql") {
					continue
				}
				volumeMatches++
				used, capacity, valid = volume.used()
				if !valid {
					return result, kubectlFailure(c, "K3", kubectlArgs(c, k3...), 0, during)
				}
				result.pvcUsed, result.pvcCapacity = used, capacity
				result.pvc = float64(used) / float64(capacity) * 100
			}
		}
	}
	if volumeMatches != 1 {
		return result, failure(4, "refused: postgres volume not found in the kubelet summary")
	}
	return result, nil
}

func evaluateReadings(current, baseline readings, limits thresholds, during bool) error {
	if during && current.restarts > baseline.restarts {
		reason := safeLastReason(current.lastReason)
		if reason == "" {
			reason = "unknown"
		}
		return failure(6, "stopped: postgres restarted (restart count %d -> %d, last reason %s); review before any resume", baseline.restarts, current.restarts, reason)
	}
	if !current.ready {
		if during {
			return failure(4, "stopped: postgres is not ready")
		}
		return failure(4, "refused: postgres is not ready")
	}
	baseMemory := make(map[string]float64, len(baseline.nodes))
	for _, node := range baseline.nodes {
		baseMemory[node.name] = node.memory
	}
	for i, node := range current.nodes {
		if node.memory >= limits.memory {
			if during {
				return failure(4, "stopped: node %d memory %.1f%% reached the stop threshold %.1f%%", i+1, node.memory, limits.memory)
			}
			return failure(4, "refused: node %d memory %.1f%% is at or above the stop threshold %.1f%%", i+1, node.memory, limits.memory)
		}
		baselineMemory, exists := baseMemory[node.name]
		if during && !exists {
			return failure(4, "stopped: cluster node membership changed")
		}
		if during && node.memory-baselineMemory >= limits.rise {
			return failure(4, "stopped: node %d memory rose %.1f points above its baseline %.1f%% (limit %.1f)", i+1, node.memory-baselineMemory, baselineMemory, limits.rise)
		}
	}
	if during && len(current.nodes) != len(baseline.nodes) {
		return failure(4, "stopped: cluster node membership changed")
	}
	if current.pvc >= limits.pvc {
		if during {
			return failure(4, "stopped: postgres volume %.1f%% reached %.1f%%", current.pvc, limits.pvc)
		}
		return failure(4, "refused: postgres volume %.1f%% is at or above %.1f%%", current.pvc, limits.pvc)
	}
	for i, node := range current.nodes {
		if node.fs >= limits.fs {
			return filesystemFailure(i+1, node.fs, limits.fs, during)
		}
	}
	return nil
}

func projectedVolume(r readings, remainingLogical int64) float64 {
	return (float64(r.pvcUsed) + float64(remainingLogical) + 1073741824) / float64(r.pvcCapacity) * 100
}

func filesystemFailure(index int, percent, limit float64, during bool) error {
	if during {
		return failure(4, "stopped: node %d filesystem %.1f%% reached %.1f%%", index, percent, limit)
	}
	return failure(4, "refused: node %d filesystem %.1f%% is at or above %.1f%%", index, percent, limit)
}

func checkFilesystemSpace(r readings, estimate int64, limit float64, during bool) error {
	for i, node := range r.nodes {
		projected := (float64(node.fsUsed) + 2*float64(estimate)) / float64(node.fsCapacity) * 100
		if projected >= limit {
			return filesystemFailure(i+1, projected, limit, during)
		}
	}
	return nil
}

func nodePercentages(r readings, memory bool) string {
	values := make([]string, len(r.nodes))
	for i, node := range r.nodes {
		value := node.fs
		if memory {
			value = node.memory
		}
		values[i] = fmt.Sprintf("%.1f%%", value)
	}
	return strings.Join(values, ", ")
}
