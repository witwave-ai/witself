package cell

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestCivoNodeCountAtTheProfileDefaultKeepsTheBaseShape(t *testing.T) {
	for _, tc := range civoShapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLOUDFLARE_API_TOKEN", "")
			count := "1"
			if tc.name == "argocd-prod-pinned-token" {
				count = "2"
			}
			tc.config["witself:civoNodeCount"] = count
			shape, err := runCivoShape(t, "civo-fixture-use1-serving", tc.config, &civoShapeMocks{})
			if err != nil {
				t.Fatal(err)
			}
			golden, err := os.ReadFile(filepath.Join("testdata", "civo-default-shape", tc.name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(marshalCivoShape(t, shape), golden) {
				compareCivoShape(t, shape, golden)
			}
		})
	}
}

func TestCivoNodeCountChangesOnlyThePoolNodeCount(t *testing.T) {
	fixtures := map[string]civoShapeCase{}
	goldens := map[string][]byte{}
	for _, fixture := range civoShapeCases() {
		fixtures[fixture.name] = fixture
		golden, err := os.ReadFile(filepath.Join("testdata", "civo-default-shape", fixture.name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var shape civoShape
		if err := json.Unmarshal(golden, &shape); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(marshalCivoShape(t, shape), golden) {
			t.Fatalf("golden %s does not round-trip byte-identically", fixture.name)
		}
		goldens[fixture.name] = golden
	}
	for _, tc := range []struct {
		fixture string
		count   int
	}{
		{"argocd-prod-pinned-token", 3},
		{"argocd-prod-pinned-token", 5},
		{"plain-minimal", 2},
		{"argocd-minimal-generated", 4},
	} {
		t.Run(tc.fixture+"/"+strconv.Itoa(tc.count), func(t *testing.T) {
			t.Setenv("CLOUDFLARE_API_TOKEN", "")
			fixture := fixtures[tc.fixture]
			fixture.config["witself:civoNodeCount"] = strconv.Itoa(tc.count)
			shape, err := runCivoShape(t, "civo-fixture-use1-serving", fixture.config, &civoShapeMocks{})
			if err != nil {
				t.Fatal(err)
			}
			golden := goldens[tc.fixture]
			var want civoShape
			if err := json.Unmarshal(golden, &want); err != nil {
				t.Fatal(err)
			}
			clusters := 0
			for i := range want.Resources {
				if want.Resources[i].Type != "civo:index/kubernetesCluster:KubernetesCluster" {
					continue
				}
				pools, ok := want.Resources[i].Inputs["pools"].(map[string]interface{})
				if !ok {
					t.Fatal("golden cluster has no pool object")
				}
				pools["nodeCount"] = float64(tc.count)
				clusters++
			}
			if clusters != 1 {
				t.Fatalf("golden has %d clusters, want 1", clusters)
			}
			got := marshalCivoShape(t, shape)
			if bytes.Equal(got, golden) {
				t.Error("configured node count did not change the golden shape")
			}
			expected := marshalCivoShape(t, want)
			if !bytes.Equal(got, expected) {
				compareCivoShape(t, shape, expected)
			}
		})
	}
}

func TestCivoNodeCountRejectedBeforeAnyResource(t *testing.T) {
	for _, tc := range []struct {
		profile string
		count   string
		minimum int
	}{
		{"prod", "1", 2},
		{"prod", "6", 2},
		{"prod", "0", 2},
		{"prod", "-1", 2},
		{"prod", "abc", 2},
		{"prod", " 3", 2},
		{"prod", "3.0", 2},
		{"minimal", "0", 1},
		{"minimal", "6", 1},
	} {
		t.Run(tc.profile+"/"+tc.count, func(t *testing.T) {
			mocks := &deletionProtectionMocks{}
			err := pulumi.RunErr(Program, pulumi.WithMocks("witself-infra", "civo-fixture-use1-dev", mocks), func(info *pulumi.RunInfo) {
				info.Config = map[string]string{
					"witself:cloud":         "civo",
					"witself:profile":       tc.profile,
					"witself:civoNodeSize":  "g4s.kube.medium",
					"witself:civoAdminCIDR": "203.0.113.7/32",
					"witself:civoNodeCount": tc.count,
					"civo:region":           "nyc1",
				}
			})
			want := fmt.Sprintf("witself:civoNodeCount %q must be a whole number from %d to 5 for profile %q", tc.count, tc.minimum, tc.profile)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want message containing %q", err, want)
			}
			mocks.registrationMu.Lock()
			defer mocks.registrationMu.Unlock()
			for _, registration := range mocks.registrations {
				if registration.TypeToken != "pulumi:pulumi:Stack" {
					t.Errorf("invalid node count registered resource %s %s", registration.TypeToken, registration.Name)
				}
			}
		})
	}
}

func TestCivoDefaultNodeCount(t *testing.T) {
	if CivoMaxNodeCount != 5 {
		t.Errorf("CivoMaxNodeCount = %d, want 5", CivoMaxNodeCount)
	}
	for _, tc := range []struct {
		profile string
		want    int
	}{
		{"prod", 2},
		{"minimal", 1},
		{"", 1},
		{"other", 1},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			if got := CivoDefaultNodeCount(tc.profile); got != tc.want {
				t.Errorf("CivoDefaultNodeCount(%q) = %d, want %d", tc.profile, got, tc.want)
			}
			got, err := civoNodeCountFromConfig(tc.profile, "")
			if err != nil || got != civoNodeProfileFor(tc.profile) {
				t.Errorf("civoNodeCountFromConfig(%q, empty) = %d, %v, want %d, nil", tc.profile, got, err, civoNodeProfileFor(tc.profile))
			}
		})
	}
}
