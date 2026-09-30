package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestParseMemoryQuantity(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  int64
	}{
		{"0", 0}, {"123", 123}, {"1Ki", 1024}, {"2Mi", 2 << 20}, {"3Gi", 3 << 30}, {"1Ti", 1 << 40},
		{"2k", 2000}, {"3M", 3000000}, {"4G", 4000000000}, {"5T", 5000000000000},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseMemoryQuantity(tc.input)
			if err != nil || got != tc.want {
				t.Errorf("quantity result = %d, valid = %t; want %d", got, err == nil, tc.want)
			}
		})
	}
	for i, input := range []string{"", "1.5Gi", "-1", "+1", "1K", "1mi", "1MiB", " 1", "1\n", "1e3", "9223372036854775808", "8388608Ti"} {
		t.Run(fmt.Sprintf("invalid-%d", i), func(t *testing.T) {
			if _, err := parseMemoryQuantity(input); err == nil {
				t.Error("invalid quantity was accepted")
			}
		})
	}
}

type clusterFixture struct {
	nodes      string
	metrics    string
	postgres   string
	summaries  map[string]string
	ingress    string
	calls      [][]string
	beforeCall func([]string)
}

func newClusterFixture() *clusterFixture {
	return &clusterFixture{
		nodes:    "node-b\t100Mi\nnode-a\t100Mi\n",
		metrics:  `{"items":[{"metadata":{"name":"node-a"},"usage":{"memory":"50Mi"}},{"metadata":{"name":"node-b"},"usage":{"memory":"60Mi"}}]}`,
		postgres: "node-a\t0\ttrue\t",
		ingress:  "localhost 127.0.0.1",
		summaries: map[string]string{
			"node-a": `{"node":{"fs":{"availableBytes":7500000000,"capacityBytes":10000000000}},"pods":[{"podRef":{"name":"witself-postgresql-0","namespace":"witself"},"volume":[{"pvcRef":{"name":"data-witself-postgresql-0","namespace":"witself"},"availableBytes":7000000000,"capacityBytes":10000000000}]}]}`,
			"node-b": `{"node":{"fs":{"availableBytes":6000000000,"capacityBytes":10000000000}},"pods":[]}`,
		},
	}
}

func (f *clusterFixture) run(_ context.Context, _ string, args []string) ([]byte, int, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.beforeCall != nil {
		f.beforeCall(args)
	}
	for i := range args {
		if args[i] == "--request-timeout=20s" {
			args = args[i+1:]
			break
		}
	}
	if len(args) == 4 && args[0] == "get" && args[1] == "nodes" {
		return []byte(f.nodes), 0, nil
	}
	if len(args) == 3 && args[0] == "get" && args[1] == "--raw" {
		if args[2] == "/apis/metrics.k8s.io/v1beta1/nodes" {
			return []byte(f.metrics), 0, nil
		}
		for name, summary := range f.summaries {
			if args[2] == "/api/v1/nodes/"+name+"/proxy/stats/summary" {
				return []byte(summary), 0, nil
			}
		}
	}
	if len(args) == 7 && args[0] == "-n" && args[3] == "pod" {
		return []byte(f.postgres), 0, nil
	}
	if len(args) == 7 && args[0] == "-n" && args[3] == "ingress" {
		return []byte(f.ingress), 0, nil
	}
	return nil, 1, errors.New("unexpected fake command")
}

func clusterConfig() config {
	return config{kubeContext: "witself-fake-cell", kubectl: "kubectl"}
}

func clusterAssertError(t *testing.T, err error, code int, message string) {
	t.Helper()
	var result *toolError
	if !errors.As(err, &result) {
		t.Fatal("missing structured failure")
	}
	if result.code != code || result.msg != message {
		t.Errorf("failure mismatch: code %d, message matches = %t", result.code, result.msg == message)
	}
}

func TestProbeComputesReadings(t *testing.T) {
	f := newClusterFixture()
	r, err := probe(context.Background(), clusterConfig(), deps{run: f.run}, false)
	if err != nil {
		t.Fatal("probe failed")
	}
	if len(r.nodes) != 2 || r.nodes[0].name != "node-a" || r.nodes[1].name != "node-b" {
		t.Fatal("nodes are not sorted")
	}
	if nodePercentages(r, true) != "50.0%, 60.0%" || nodePercentages(r, false) != "25.0%, 40.0%" || fmt.Sprintf("%.1f", r.pvc) != "30.0" {
		t.Error("readings did not use allocatable memory and available capacity")
	}
	if !r.ready || r.restarts != 0 || r.pvcCapacity != 10000000000 || r.pvcUsed != 3000000000 {
		t.Error("postgres readings differ")
	}
	for _, tc := range []struct {
		name string
		edit func(*clusterFixture)
	}{
		{"missing-node-metrics", func(f *clusterFixture) { f.metrics = `{"items":[]}` }},
		{"duplicate-node", func(f *clusterFixture) { f.nodes += "node-a\t100Mi\n" }},
		{"malformed-quantity", func(f *clusterFixture) { f.nodes = "node-a\t1.2Gi\n" }},
		{"missing-capacity", func(f *clusterFixture) { f.summaries["node-b"] = `{"node":{"fs":{"availableBytes":1}}}` }},
		{"invalid-readiness", func(f *clusterFixture) { f.postgres = "node-a\t0\tmaybe\t" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClusterFixture()
			tc.edit(f)
			if _, err := probe(context.Background(), clusterConfig(), deps{run: f.run}, false); err == nil {
				t.Error("invalid probe accepted")
			}
		})
	}
}

func TestProbeArgvAllowList(t *testing.T) {
	for _, kubeconfig := range []string{"", "<kubeconfig>"} {
		t.Run(fmt.Sprintf("config-%t", kubeconfig != ""), func(t *testing.T) {
			f := newClusterFixture()
			c := clusterConfig()
			c.kubeconfig = kubeconfig
			if _, err := probe(context.Background(), c, deps{run: f.run}, false); err != nil {
				t.Fatal("probe failed")
			}
			if _, err := ingressHosts(context.Background(), c, deps{run: f.run}); err != nil {
				t.Fatal("ingress read failed")
			}
			prefix := []string{"--context", "witself-fake-cell", "--request-timeout=20s"}
			if kubeconfig != "" {
				prefix = append([]string{"--kubeconfig", kubeconfig}, prefix...)
			}
			forms := [][]string{
				{"get", "nodes", "-o", `jsonpath={range .items[*]}{.metadata.name}{"\t"}{.status.allocatable.memory}{"\n"}{end}`},
				{"get", "--raw", "/apis/metrics.k8s.io/v1beta1/nodes"},
				{"-n", "witself", "get", "pod", "witself-postgresql-0", "-o", `jsonpath={.spec.nodeName}{"\t"}{.status.containerStatuses[?(@.name=="postgresql")].restartCount}{"\t"}{.status.containerStatuses[?(@.name=="postgresql")].ready}{"\t"}{.status.containerStatuses[?(@.name=="postgresql")].lastState.terminated.reason}`},
				{"get", "--raw", "/api/v1/nodes/node-a/proxy/stats/summary"},
				{"get", "--raw", "/api/v1/nodes/node-b/proxy/stats/summary"},
				{"-n", "witself", "get", "ingress", "witself-server", "-o", `jsonpath={.spec.rules[*].host}`},
			}
			if len(f.calls) != len(forms) {
				t.Fatalf("got %d commands, want %d", len(f.calls), len(forms))
			}
			for i, form := range forms {
				want := append(append([]string(nil), prefix...), form...)
				if !reflect.DeepEqual(f.calls[i], want) {
					t.Errorf("command %d differs from allow-list", i)
				}
			}
		})
	}
}

func TestProbeRejectsUnsafeNodeNames(t *testing.T) {
	for i, name := range []string{"bad/name", "..", "node..name", "-node", "node-", "Node", "node;command", strings.Repeat("a", 254)} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			f := newClusterFixture()
			f.nodes = name + "\t100Mi\n"
			_, err := probe(context.Background(), clusterConfig(), deps{run: f.run}, false)
			clusterAssertError(t, err, 4, "refused: unsafe node name from kubectl")
			if len(f.calls) != 1 {
				t.Error("unsafe name reached another command")
			}
		})
	}
}

func TestProbeFailureMessageIsValueFree(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kubeconfig string
		quoted     string
	}{
		{"single-quote", "operator's config", `'operator'\''s config'`},
		{"identifier-substring", "/tmp/client_x.yaml", `'/tmp/client_x.yaml'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clusterConfig()
			c.kubeconfig = tc.kubeconfig
			d := deps{run: func(context.Context, string, []string) ([]byte, int, error) {
				return []byte("test-other-token-not-real"), 1, errors.New("test-other-token-not-real")
			}}
			_, err := probe(context.Background(), c, d, false)
			want := `refused: kubectl step K1 failed (exit 1); re-run: 'kubectl' '--kubeconfig' ` + tc.quoted + ` '--context' 'witself-fake-cell' '--request-timeout=20s' 'get' 'nodes' '-o' 'jsonpath={range .items[*]}{.metadata.name}{"\t"}{.status.allocatable.memory}{"\n"}{end}'`
			clusterAssertError(t, err, 4, want)
			if strings.Contains(err.Error(), "test-other-token-not-real") {
				t.Error("private subprocess text escaped")
			}
			_, err = probe(context.Background(), c, d, true)
			clusterAssertError(t, err, 4, "stopped: kubectl step K1 failed (exit 1)")
		})
	}
}

func healthyReadings() readings {
	return readings{nodes: []nodeReading{{name: "node-a", memory: 60, fs: 25, fsUsed: 25, fsCapacity: 100}}, pvc: 30, pvcUsed: 30, pvcCapacity: 100, ready: true}
}

func TestThresholdEvaluation(t *testing.T) {
	limits := thresholds{memory: 90, rise: 5, pvc: 70, fs: 75}
	for _, tc := range []struct {
		name    string
		during  bool
		code    int
		edit    func(*readings)
		message string
	}{
		{"memory-start", false, 4, func(r *readings) { r.nodes[0].memory = 90 }, "refused: node 1 memory 90.0% is at or above the stop threshold 90.0%"},
		{"memory-stop", true, 4, func(r *readings) { r.nodes[0].memory = 90 }, "stopped: node 1 memory 90.0% reached the stop threshold 90.0%"},
		{"memory-rise", true, 4, func(r *readings) { r.nodes[0].memory = 65 }, "stopped: node 1 memory rose 5.0 points above its baseline 60.0% (limit 5.0)"},
		{"volume-start", false, 4, func(r *readings) { r.pvc = 70 }, "refused: postgres volume 70.0% is at or above 70.0%"},
		{"volume-stop", true, 4, func(r *readings) { r.pvc = 70 }, "stopped: postgres volume 70.0% reached 70.0%"},
		{"filesystem-start", false, 4, func(r *readings) { r.nodes[0].fs = 75 }, "refused: node 1 filesystem 75.0% is at or above 75.0%"},
		{"filesystem-stop", true, 4, func(r *readings) { r.nodes[0].fs = 75 }, "stopped: node 1 filesystem 75.0% reached 75.0%"},
		{"not-ready-start", false, 4, func(r *readings) { r.ready = false }, "refused: postgres is not ready"},
		{"not-ready-stop", true, 4, func(r *readings) { r.ready = false }, "stopped: postgres is not ready"},
		{"restart", true, 6, func(r *readings) { r.restarts = 1; r.lastReason = "OOMKilled"; r.ready = false }, "stopped: postgres restarted (restart count 0 -> 1, last reason OOMKilled); review before any resume"},
		{"restart-unknown", true, 6, func(r *readings) { r.restarts = 1 }, "stopped: postgres restarted (restart count 0 -> 1, last reason unknown); review before any resume"},
		{"topology-changed", true, 4, func(r *readings) { r.nodes[0].name = "new-node" }, "stopped: cluster node membership changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseline, current := healthyReadings(), healthyReadings()
			tc.edit(&current)
			clusterAssertError(t, evaluateReadings(current, baseline, limits, tc.during), tc.code, tc.message)
		})
	}
	baseline := healthyReadings()
	if err := evaluateReadings(baseline, baseline, limits, false); err != nil {
		t.Error("healthy readings refused")
	}
	baseline.pvcUsed, baseline.pvcCapacity = 1073741824, 8589934592
	if got := projectedVolume(baseline, 1073741824); got != 37.5 {
		t.Errorf("projection = %.1f, want 37.5", got)
	}
	for _, during := range []bool{false, true} {
		r := healthyReadings()
		err := checkFilesystemSpace(r, 25, 75, during)
		message := "refused: node 1 filesystem 75.0% is at or above 75.0%"
		if during {
			message = "stopped: node 1 filesystem 75.0% reached 75.0%"
		}
		clusterAssertError(t, err, 4, message)
		if err := checkFilesystemSpace(r, 24, 75, during); err != nil {
			t.Error("space below threshold refused")
		}
	}
}

func TestThresholdFlagRanges(t *testing.T) {
	for _, tc := range []struct {
		flag    string
		values  []string
		message string
	}{
		{"--max-node-memory-percent", []string{"49", "96", "NaN", "+Inf"}, "--max-node-memory-percent must be between 50 and 95"},
		{"--max-node-memory-rise", []string{"0", "11"}, "--max-node-memory-rise must be between 1 and 10"},
		{"--max-pvc-percent", []string{"29", "76"}, "--max-pvc-percent must be between 30 and 75"},
		{"--max-node-fs-percent", []string{"29", "79"}, "--max-node-fs-percent must be between 30 and 78"},
	} {
		for _, value := range tc.values {
			t.Run(tc.flag+"/"+value, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				d := deps{stdout: &stdout, stderr: &stderr, client: newHTTPClient(), run: func(context.Context, string, []string) ([]byte, int, error) {
					t.Error("invalid flags invoked subprocess")
					return nil, 1, nil
				}}
				code := cliWithOptions(context.Background(), []string{"check", "--kube-context", "witself-fake-cell", tc.flag, value}, d, productionOptions())
				if code != 2 || stderr.String() != tc.message+"\n" || stdout.Len() != 0 {
					t.Errorf("range refusal mismatch: code=%d, message matches=%t", code, stderr.String() == tc.message+"\n")
				}
			})
		}
	}
}

func TestPostgresVolumeSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(string) string
		good bool
	}{
		{"zero", func(s string) string { return strings.ReplaceAll(s, "data-witself-postgresql-0", "other-volume") }, false},
		{"two", func(s string) string {
			volume := `{"pvcRef":{"name":"data-witself-postgresql-0","namespace":"witself"},"availableBytes":7000000000,"capacityBytes":10000000000}`
			return strings.Replace(s, volume, volume+","+volume, 1)
		}, false},
		{"ignored-other-namespace", func(s string) string {
			volume := `{"pvcRef":{"name":"data-witself-postgresql-0","namespace":"witself"},"availableBytes":7000000000,"capacityBytes":10000000000}`
			extra := strings.Replace(volume, `"namespace":"witself"`, `"namespace":"other"`, 1)
			return strings.Replace(s, volume, volume+","+extra, 1)
		}, true},
		{"wrong-pod-namespace", func(s string) string { return strings.Replace(s, `"namespace":"witself"`, `"namespace":"other"`, 1) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClusterFixture()
			f.summaries["node-a"] = tc.edit(f.summaries["node-a"])
			_, err := probe(context.Background(), clusterConfig(), deps{run: f.run}, false)
			if tc.good {
				if err != nil {
					t.Error("unrelated volume prevented selection")
				}
			} else {
				clusterAssertError(t, err, 4, "refused: postgres volume not found in the kubelet summary")
			}
		})
	}
}

func fakeKubectlMain(_ []string) int {
	if _, err := os.Stdout.WriteString("fixture kubectl stdout\n"); err != nil {
		return 2
	}
	if _, err := os.Stderr.WriteString("test-other-token-not-real"); err != nil {
		return 2
	}
	code, err := strconv.Atoi(os.Getenv("FIXTURE_LOADER_FAKE_KUBECTL_EXIT"))
	if err != nil {
		return 2
	}
	return code
}

func TestExecRunnerHelperProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate test executable")
	}
	for _, wantCode := range []int{0, 3} {
		t.Run(strconv.Itoa(wantCode), func(t *testing.T) {
			t.Setenv("FIXTURE_LOADER_FAKE_KUBECTL", "1")
			t.Setenv("FIXTURE_LOADER_FAKE_KUBECTL_EXIT", strconv.Itoa(wantCode))
			c := clusterConfig()
			c.kubectl = executable
			stdout, code, err := execRunner(context.Background(), c.kubectl, kubectlArgs(c, "get", "nodes", "-o", nodesJSONPath))
			if string(stdout) != "fixture kubectl stdout\n" || code != wantCode || (err != nil) != (wantCode != 0) {
				t.Errorf("helper result mismatch: code=%d, stdout matches=%t", code, string(stdout) == "fixture kubectl stdout\n")
			}
			if bytes.Contains(stdout, []byte("test-other-token-not-real")) {
				t.Error("subprocess private stderr escaped")
			}
		})
	}
}
