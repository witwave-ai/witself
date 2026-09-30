package cell

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
)

func TestCivoLoadBalancerAddress(t *testing.T) {
	pointer := func(value string) *string { return &value }
	status := func(entries ...corev1.LoadBalancerIngress) *corev1.ServiceStatus {
		return &corev1.ServiceStatus{LoadBalancer: &corev1.LoadBalancerStatus{Ingress: entries}}
	}
	for _, test := range []struct {
		name   string
		status *corev1.ServiceStatus
		want   string
	}{
		{name: "nil status"},
		{name: "nil load balancer", status: &corev1.ServiceStatus{}},
		{name: "empty list", status: status()},
		{name: "host name only", status: status(corev1.LoadBalancerIngress{Hostname: pointer("fixture.lb.civo.test")})},
		{name: "IPv6 only", status: status(corev1.LoadBalancerIngress{Ip: pointer("2001:db8::20")})},
		{name: "mapped IPv6 only", status: status(corev1.LoadBalancerIngress{Ip: pointer("::ffff:203.0.113.20")})},
		{name: "empty address", status: status(corev1.LoadBalancerIngress{Ip: pointer("")})},
		{name: "surrounding spaces", status: status(corev1.LoadBalancerIngress{Ip: pointer(" 203.0.113.20 ")})},
		{name: "host name before IPv4", status: status(corev1.LoadBalancerIngress{Hostname: pointer("fixture.lb.civo.test")}, corev1.LoadBalancerIngress{Ip: pointer("203.0.113.20")}), want: "203.0.113.20"},
		{name: "plain IPv4", status: status(corev1.LoadBalancerIngress{Ip: pointer("203.0.113.20")}), want: "203.0.113.20"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := civoLoadBalancerAddress(test.status)
			if test.want == "" {
				want := "load balancer Service kube-system/witself-lb reports no IPv4 address in status.loadBalancer.ingress; the host name in that status is never used"
				if err == nil || err.Error() != want || got != "" {
					t.Fatalf("address refusal = %q, %v; want empty address and exact refusal", got, err)
				}
			} else if err != nil || got != test.want {
				t.Fatalf("address = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestValidateCivoIngress(t *testing.T) {
	for _, test := range []struct {
		name string
		cell civoCell
		want string
	}{
		{name: "unknown ingress wins", cell: civoCell{ingress: "other", dns: "other"}, want: `witself:civoIngress "other" is not supported (want nodeport or loadbalancer)`},
		{name: "unknown DNS", cell: civoCell{dns: "other"}, want: `witself:civoDNS "other" is not supported (want none or cloudflare)`},
		{name: "DNS needs load balancer", cell: civoCell{dns: "cloudflare"}, want: "witself:civoDNS cloudflare requires witself:civoIngress loadbalancer"},
		{name: "domain required", cell: civoCell{ingress: "loadbalancer"}, want: "witself:domain is required with witself:civoIngress loadbalancer"},
		{name: "normalized empty domain", cell: civoCell{ingress: "loadbalancer", domain: " . "}, want: "witself:domain is required with witself:civoIngress loadbalancer"},
		{name: "defaults"},
		{name: "nodeport none", cell: civoCell{ingress: "nodeport", dns: "none"}},
		{name: "load balancer none", cell: civoCell{ingress: "loadbalancer", dns: "none", domain: "cells.example.test"}},
		{name: "load balancer Cloudflare", cell: civoCell{ingress: "loadbalancer", dns: "cloudflare", domain: "cells.example.test"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateCivoIngress(test.cell)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != test.want {
				t.Fatalf("validation = %v, want %s", err, test.want)
			}
		})
	}
}

func TestCivoLoadBalancerShape(t *testing.T) {
	base := civoIngressShape(t, civoShapeCases()[0].config, &civoShapeMocks{})
	mocks := &civoShapeMocks{}
	shape := civoIngressShape(t, civoIngressConfig("none"), mocks)
	assertCivoAddedPairs(t, base, shape, "kubernetes:core/v1:Service / civo-ingress-lb")
	for _, old := range base.Resources {
		if old.Name == "argocd-root" {
			continue
		}
		got := civoIngressRecord(t, shape, old.Type, old.Name)
		if !reflect.DeepEqual(got, old) {
			t.Errorf("base record changed: %s / %s", old.Type, old.Name)
		}
	}
	service := civoIngressRecord(t, shape, "kubernetes:core/v1:Service", "civo-ingress-lb")
	wantInputs := map[string]interface{}{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]interface{}{
			"name": "witself-lb", "namespace": "kube-system",
			"annotations": map[string]interface{}{"kubernetes.civo.com/firewall-id": "cell-firewall-id"},
		},
		"spec": map[string]interface{}{
			"type":     "LoadBalancer",
			"selector": map[string]interface{}{"app.kubernetes.io/instance": "traefik-kube-system", "app.kubernetes.io/name": "traefik"},
			"ports": []interface{}{
				map[string]interface{}{"name": "http", "protocol": "TCP", "port": float64(80), "targetPort": float64(80)},
				map[string]interface{}{"name": "https", "protocol": "TCP", "port": float64(443), "targetPort": float64(443)},
			},
		},
	}
	metadata := civoIngressObject(t, service.Inputs["metadata"])
	if !reflect.DeepEqual(metadata["annotations"], map[string]interface{}{"kubernetes.civo.com/firewall-id": "cell-firewall-id"}) {
		t.Error("Service annotations must contain only kubernetes.civo.com/firewall-id with the cell firewall id")
	}
	if !reflect.DeepEqual(service.Inputs, wantInputs) {
		t.Error("Service manifest differs from the exact required inputs")
	}
	if service.Provider != civoIngressURN("pulumi:providers:kubernetes", "cell-k8s")+"::cell-k8s-id" {
		t.Error("Service does not use the cell-k8s provider")
	}
	assertCivoDependencies(t, service, civoIngressURN("civo:index/kubernetesCluster:KubernetesCluster", "civo-cluster"), civoIngressURN("civo:index/firewall:Firewall", "cell-firewall"))
	if len(service.IgnoreChanges) != 0 || !reflect.DeepEqual(service.CustomTimeouts, map[string]string{"create": "", "update": "", "delete": ""}) || (service.Protect != nil && *service.Protect) {
		t.Error("Service must have no ignored changes, custom timeouts or protection")
	}
	serviceCount := 0
	for _, item := range shape.Resources {
		if item.Type == "kubernetes:core/v1:Service" {
			serviceCount++
		}
		for _, forbidden := range []string{"reservedIp", "kubernetesNodePool", "loadBalancer"} {
			if strings.Contains(item.Type, forbidden) {
				t.Errorf("unexpected additional infrastructure: %s / %s", item.Type, item.Name)
			}
		}
		encoded, err := json.Marshal(item.Inputs)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "lb.civo.test") {
			t.Errorf("load balancer status host name reached inputs of %s / %s", item.Type, item.Name)
		}
	}
	if serviceCount != 1 {
		t.Errorf("Service count = %d, want 1", serviceCount)
	}
	cluster := civoIngressRecord(t, shape, "civo:index/kubernetesCluster:KubernetesCluster", "civo-cluster")
	pool := civoIngressObject(t, cluster.Inputs["pools"])
	if pool["nodeCount"] != float64(2) || pool["size"] != "g4s.kube.medium" {
		t.Error("load balancer changed the existing node pool")
	}
	root := civoIngressRecord(t, shape, "kubernetes:argoproj.io/v1alpha1:Application", "argocd-root")
	assertCivoDependencies(t, root, civoIngressURN("kubernetes:core/v1:Service", "civo-ingress-lb"))
	assertCivoInlineHost(t, root, "civo-fixture-use1-serving.cells.example.test", "api.civo-fixture-use1-serving.cells.example.test")
	assertCivoExportKeys(t, base.Exports, shape.Exports, "civoDNS", "civoIngress", "loadBalancerIP")
	assertCivoExportValue(t, shape, "apiHost", "api.civo-fixture-use1-serving.cells.example.test")
	assertCivoExportValue(t, shape, "loadBalancerIP", "203.0.113.20")
	assertCivoExportValue(t, shape, "civoDNSEntry", "fixture-cluster.k8s.civo.test")
	if len(mocks.zoneLookups) != 0 {
		t.Error("operator-managed DNS performed a zone lookup")
	}
}

func TestCivoCloudflareRecordShape(t *testing.T) {
	base := civoIngressShape(t, civoShapeCases()[0].config, &civoShapeMocks{})
	mocks := &civoShapeMocks{zoneName: "example.test", zoneID: "zone-id-fixture"}
	shape := civoIngressShape(t, civoIngressConfig("cloudflare"), mocks)
	assertCivoAddedPairs(t, base, shape,
		"kubernetes:core/v1:Service / civo-ingress-lb",
		"pulumi:providers:cloudflare / cloudflare",
		"cloudflare:index/dnsRecord:DnsRecord / cell-api-record")
	record := civoIngressRecord(t, shape, "cloudflare:index/dnsRecord:DnsRecord", "cell-api-record")
	want := map[string]interface{}{
		"zoneId": "zone-id-fixture", "name": "api.civo-fixture-use1-serving.cells.example.test", "type": "A",
		"content": "203.0.113.20", "ttl": float64(300), "proxied": false, "comment": "Witself cell API: civo-fixture-use1-serving",
	}
	if !reflect.DeepEqual(record.Inputs, want) {
		t.Error("Cloudflare record differs from its exact seven required inputs")
	}
	if record.Provider != civoIngressURN("pulumi:providers:cloudflare", "cloudflare")+"::cloudflare-id" {
		t.Error("record does not use the Cloudflare provider")
	}
	assertCivoDependencies(t, record, civoIngressURN("kubernetes:core/v1:Service", "civo-ingress-lb"))
	mocks.registrationMu.Lock()
	for _, args := range mocks.registrations {
		if args.TypeToken == "cloudflare:index/dnsRecord:DnsRecord" && args.Name == "cell-api-record" && args.RegisterRPC.GetVersion() != "6.17.0" {
			t.Errorf("Cloudflare record version = %q, want 6.17.0", args.RegisterRPC.GetVersion())
		}
	}
	mocks.registrationMu.Unlock()
	if !reflect.DeepEqual(mocks.zoneLookups, []string{"cells.example.test", "example.test"}) {
		t.Errorf("zone lookup order = %v", mocks.zoneLookups)
	}
	root := civoIngressRecord(t, shape, "kubernetes:argoproj.io/v1alpha1:Application", "argocd-root")
	assertCivoDependencies(t, root, civoIngressURN("kubernetes:core/v1:Service", "civo-ingress-lb"), civoIngressURN("cloudflare:index/dnsRecord:DnsRecord", "cell-api-record"))
	assertCivoExportKeys(t, base.Exports, shape.Exports, "civoDNS", "civoIngress", "loadBalancerIP", "cloudflareDNSZone")
	assertCivoExportValue(t, shape, "cloudflareDNSZone", "example.test")
}

func TestCivoLoadBalancerWithoutArgoCD(t *testing.T) {
	config := civoShapeCases()[2].config
	base := civoIngressShape(t, config, &civoShapeMocks{})
	config["witself:civoIngress"] = "loadbalancer"
	config["witself:civoDNS"] = "none"
	config["witself:domain"] = "cells.example.test"
	shape := civoIngressShape(t, config, &civoShapeMocks{})
	assertCivoAddedPairs(t, base, shape, "pulumi:providers:kubernetes / cell-k8s", "kubernetes:core/v1:Service / civo-ingress-lb")
	if len(shape.Resources) != 5 {
		t.Errorf("resource count = %d, want 5", len(shape.Resources))
	}
	assertCivoExportKeys(t, base.Exports, shape.Exports, "civoDNS", "civoIngress", "loadBalancerIP")
	assertCivoExportValue(t, shape, "apiHost", "api.civo-fixture-use1-serving.cells.example.test")
}

func TestCivoProductionHostIsPinned(t *testing.T) {
	config := civoIngressConfig("cloudflare")
	config["witself:gitopsValuesPath"] = ".gitops/cells/civo-prod-use1-serving/values.yaml"
	config["witself:domain"] = "cells.witself.witwave.ai"
	mocks := &civoShapeMocks{zoneName: "witwave.ai", zoneID: "zone-id-fixture"}
	shape, err := runCivoShape(t, "civo-prod-use1-serving", config, mocks)
	if err != nil {
		t.Fatal(err)
	}
	assertCivoExportValue(t, shape, "apiHost", "api.civo-prod-use1-serving.cells.witself.witwave.ai")
	record := civoIngressRecord(t, shape, "cloudflare:index/dnsRecord:DnsRecord", "cell-api-record")
	if record.Inputs["name"] != "api.civo-prod-use1-serving.cells.witself.witwave.ai" {
		t.Error("production DNS record host is not the pinned literal")
	}
	root := civoIngressRecord(t, shape, "kubernetes:argoproj.io/v1alpha1:Application", "argocd-root")
	assertCivoInlineHost(t, root, "civo-prod-use1-serving.cells.witself.witwave.ai", "api.civo-prod-use1-serving.cells.witself.witwave.ai")
	if !reflect.DeepEqual(mocks.zoneLookups, []string{"cells.witself.witwave.ai", "witself.witwave.ai", "witwave.ai"}) {
		t.Errorf("production zone lookup order = %v", mocks.zoneLookups)
	}
	cluster := civoIngressRecord(t, shape, "civo:index/kubernetesCluster:KubernetesCluster", "civo-cluster")
	if cluster.Inputs["name"] != "witself-civo-prod-use1-serving" {
		t.Error("production cluster name is not the pinned literal")
	}
}

func TestCivoLoadBalancerRequiresAddress(t *testing.T) {
	status := resource.NewObjectProperty(resource.NewPropertyMapFromMap(map[string]interface{}{
		"loadBalancer": map[string]interface{}{"ingress": []interface{}{map[string]interface{}{"hostname": "fixture.lb.civo.test"}}},
	}))
	mocks := &civoShapeMocks{zoneName: "example.test", zoneID: "zone-id-fixture", serviceStatus: &status}
	shape, err := runCivoShape(t, "civo-fixture-use1-serving", civoIngressConfig("cloudflare"), mocks)
	if err == nil || !strings.Contains(err.Error(), "reports no IPv4 address") {
		t.Fatalf("missing-address error = %v, want reports no IPv4 address", err)
	}
	assertCivoNoDNSRecord(t, shape)
}

func TestCivoCloudflareZoneMustContainHost(t *testing.T) {
	mocks := &civoShapeMocks{zoneName: "example.test", zoneID: "zone-id-fixture", zoneAnswerName: "other.test"}
	shape, err := runCivoShape(t, "civo-fixture-use1-serving", civoIngressConfig("cloudflare"), mocks)
	if err == nil || !strings.Contains(err.Error(), `zone "other.test" found in Cloudflare does not contain the host`) {
		t.Fatalf("zone containment error = %v", err)
	}
	assertCivoNoDNSRecord(t, shape)
}

func TestProgramCivoRejectsBadIngressConfig(t *testing.T) {
	for _, test := range []struct {
		name, key, value, want string
	}{
		{name: "unknown ingress", key: "witself:civoIngress", value: "other", want: `witself:civoIngress "other" is not supported (want nodeport or loadbalancer)`},
		{name: "unknown DNS", key: "witself:civoDNS", value: "other", want: `witself:civoDNS "other" is not supported (want none or cloudflare)`},
		{name: "DNS without load balancer", key: "witself:civoDNS", value: "cloudflare", want: "witself:civoDNS cloudflare requires witself:civoIngress loadbalancer"},
		{name: "load balancer without domain", key: "witself:civoIngress", value: "loadbalancer", want: "witself:domain is required with witself:civoIngress loadbalancer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := civoShapeCases()[0].config
			config[test.key] = test.value
			shape, err := runCivoShape(t, "civo-fixture-use1-serving", config, &civoShapeMocks{})
			// Pulumi wraps a program failure in a multi-error. Require exactly
			// one wrapped error and compare its message without weakening it.
			wrapped, ok := err.(interface{ WrappedErrors() []error })
			if !ok || len(wrapped.WrappedErrors()) != 1 || wrapped.WrappedErrors()[0].Error() != test.want {
				t.Fatalf("program refusal must contain exactly the error %q", test.want)
			}
			for _, item := range shape.Resources {
				if item.Custom {
					t.Errorf("refused configuration registered custom resource %s / %s", item.Type, item.Name)
				}
			}
		})
	}
}

func civoIngressConfig(dns string) map[string]string {
	config := civoShapeCases()[0].config
	config["witself:civoIngress"] = "loadbalancer"
	config["witself:civoDNS"] = dns
	config["witself:domain"] = "cells.example.test"
	return config
}

func civoIngressShape(t *testing.T, config map[string]string, mocks *civoShapeMocks) civoShape {
	t.Helper()
	shape, err := runCivoShape(t, "civo-fixture-use1-serving", config, mocks)
	if err != nil {
		t.Fatal(err)
	}
	return shape
}

func civoIngressRecord(t *testing.T, shape civoShape, typ, name string) civoShapeRecord {
	t.Helper()
	for _, item := range shape.Resources {
		if item.Type == typ && item.Name == name {
			return item
		}
	}
	t.Fatalf("missing resource %s / %s", typ, name)
	return civoShapeRecord{}
}

func civoIngressObject(t *testing.T, value interface{}) map[string]interface{} {
	t.Helper()
	result, ok := value.(map[string]interface{})
	if !ok {
		t.Fatal("expected a manifest object")
	}
	return result
}

func civoIngressURN(typ, name string) string {
	return "urn:pulumi:civo-fixture-use1-serving::witself-infra::" + typ + "::" + name
}

func assertCivoDependencies(t *testing.T, record civoShapeRecord, required ...string) {
	t.Helper()
	for _, want := range required {
		found := false
		for _, dependency := range record.Dependencies {
			if dependency == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s / %s lacks dependency %s", record.Type, record.Name, want)
		}
	}
}

func assertCivoAddedPairs(t *testing.T, base, got civoShape, added ...string) {
	t.Helper()
	want := append([]string{}, added...)
	for _, item := range base.Resources {
		want = append(want, item.Type+" / "+item.Name)
	}
	actual := []string{}
	for _, item := range got.Resources {
		actual = append(actual, item.Type+" / "+item.Name)
	}
	sort.Strings(want)
	sort.Strings(actual)
	if !reflect.DeepEqual(actual, want) {
		t.Errorf("resource pairs = %v, want %v", actual, want)
	}
}

func assertCivoExportKeys(t *testing.T, base, actual []string, added ...string) {
	t.Helper()
	want := append(append([]string{}, base...), added...)
	sort.Strings(want)
	if !reflect.DeepEqual(actual, want) {
		t.Errorf("export keys = %v, want %v", actual, want)
	}
}

func assertCivoExportValue(t *testing.T, shape civoShape, key, want string) {
	t.Helper()
	if shape.Values[key] != want {
		t.Errorf("export %s differs from expected public value %q", key, want)
	}
}

func assertCivoInlineHost(t *testing.T, root civoShapeRecord, domain, host string) {
	t.Helper()
	spec := civoIngressObject(t, root.Inputs["spec"])
	sources, ok := spec["sources"].([]interface{})
	if !ok || len(sources) == 0 {
		t.Fatal("root Application has no sources")
	}
	source := civoIngressObject(t, sources[0])
	helm := civoIngressObject(t, source["helm"])
	values, ok := helm["values"].(string)
	if !ok {
		t.Fatal("root Application has no inline values")
	}
	for _, line := range []string{"  domain: \"" + domain + "\"", "  apiHost: \"" + host + "\""} {
		if !strings.Contains(values, line+"\n") {
			t.Errorf("root Application inline values lack %s", line)
		}
	}
}

func assertCivoNoDNSRecord(t *testing.T, shape civoShape) {
	t.Helper()
	for _, item := range shape.Resources {
		if item.Type == "cloudflare:index/dnsRecord:DnsRecord" {
			t.Errorf("refused configuration registered DNS record %s", item.Name)
		}
	}
}
