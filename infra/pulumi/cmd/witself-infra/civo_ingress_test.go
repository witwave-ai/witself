package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/witwave-ai/witself/infra/pulumi/internal/backend"
	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

const civoMissingCloudflareMessage = "civo_dns cloudflare: missing environment variable CLOUDFLARE_API_TOKEN; export it in the shell that runs witself-infra (see the README for the token's permissions). It is never read from infra.yaml, a flag or a file"

func civoIngressTestHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("CIVO_TOKEN", "test-civo-token-not-real")
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
}

func assertCivoIngressError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatal("unexpected error; error values are omitted to protect credentials")
		}
		return
	}
	if err == nil || err.Error() != want {
		t.Fatalf("error mismatch, want exactly %q; received values are omitted", want)
	}
}

func assertCivoIngressPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("refused operation created a path or its absence could not be verified")
	}
}

func TestValidateCivoIngressFlags(t *testing.T) {
	base := civoIngressOptions{
		Ingress: "nodeport", DNS: "none", Domain: "cells.example.test",
		NodeSize: "g4s.kube.medium", CellName: "civo-fixture-use1-serving",
	}
	tests := []struct {
		name   string
		change func(*civoIngressOptions)
		want   string
	}{
		{
			name: "unknown ingress",
			change: func(o *civoIngressOptions) {
				o.Ingress = "other"
			},
			want: `unknown -civo-ingress "other" (want nodeport|loadbalancer)`,
		},
		{
			name: "unknown DNS",
			change: func(o *civoIngressOptions) {
				o.DNS = "other"
			},
			want: `unknown -civo-dns "other" (want none|cloudflare)`,
		},
		{
			name: "DNS requires load balancer",
			change: func(o *civoIngressOptions) {
				o.DNS = "cloudflare"
			},
			want: "-civo-dns cloudflare requires -civo-ingress loadbalancer",
		},
		{
			name: "domain required",
			change: func(o *civoIngressOptions) {
				o.Ingress, o.Domain = "loadbalancer", ""
			},
			want: "-civo-ingress loadbalancer requires -domain, the parent domain of the host api.<cell>.<domain>",
		},
		{
			name: "cell label too long",
			change: func(o *civoIngressOptions) {
				o.Ingress, o.CellName = "loadbalancer", strings.Repeat("z", 64)
			},
			want: fmt.Sprintf("cell name %q is longer than 63 characters and cannot be a label of the host api.<cell>.<domain>", strings.Repeat("z", 64)),
		},
		{
			name: "small node refused",
			change: func(o *civoIngressOptions) {
				o.NodeSize = "g4s.kube.small"
			},
			want: `-civo-node-size "g4s.kube.small" is not supported for -cloud civo (want g4s.kube.medium or g4s.kube.large)`,
		},
		{
			name: "empty node refused",
			change: func(o *civoIngressOptions) {
				o.NodeSize = ""
			},
			want: `-civo-node-size "" is not supported for -cloud civo (want g4s.kube.medium or g4s.kube.large)`,
		},
		{
			name: "large node accepted",
			change: func(o *civoIngressOptions) {
				o.NodeSize = "g4s.kube.large"
			},
		},
		{
			name: "load balancer with DNS accepted",
			change: func(o *civoIngressOptions) {
				o.Ingress, o.DNS = "loadbalancer", "cloudflare"
				o.CellName = strings.Repeat("z", 63)
			},
		},
	}
	for _, cmd := range []string{"up", "preview", "add-cell"} {
		t.Run(cmd, func(t *testing.T) {
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					o := base
					test.change(&o)
					assertCivoIngressError(t, validateCivoIngress(cmd, o), test.want)
				})
			}
		})
	}
	for _, cmd := range []string{"outputs", "cell-health", "refresh", "destroy"} {
		t.Run(cmd, func(t *testing.T) {
			o := base
			o.Ingress, o.Domain, o.NodeSize = "loadbalancer", "", ""
			o.CellName = strings.Repeat("z", 64)
			assertCivoIngressError(t, validateCivoIngress(cmd, o), "")
			for _, test := range tests[:3] {
				t.Run(test.name, func(t *testing.T) {
					o := base
					test.change(&o)
					assertCivoIngressError(t, validateCivoIngress(cmd, o), test.want)
				})
			}
		})
	}
}

func TestCivoIngressRunRefusals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown ingress", []string{"-civo-ingress", "other"}, `unknown -civo-ingress "other" (want nodeport|loadbalancer)`},
		{"unknown DNS", []string{"-civo-dns", "other"}, `unknown -civo-dns "other" (want none|cloudflare)`},
		{"default domain refusal", []string{"-domain", "cells.example.test"}, "-domain does not apply to -cloud civo; Civo uses provider networking, native DNS, and in-cluster PostgreSQL"},
		{"custom domain accepted", []string{"-civo-ingress", "loadbalancer", "-domain", "cells.example.test"}, "-civo-admin-cidr is required with -cloud civo"},
		{"empty domain", []string{"-civo-ingress", "loadbalancer", "-domain", ""}, "-civo-ingress loadbalancer requires -domain, the parent domain of the host api.<cell>.<domain>"},
		{"small node", []string{"-civo-node-size", "g4s.kube.small"}, `-civo-node-size "g4s.kube.small" is not supported for -cloud civo (want g4s.kube.medium or g4s.kube.large)`},
		{"large node accepted", []string{"-civo-node-size", "g4s.kube.large"}, "-civo-admin-cidr is required with -cloud civo"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			civoIngressTestHome(t)
			state := filepath.Join(t.TempDir(), "state")
			args := []string{"preview", "-cloud", "civo", "-region", "nyc1", "-backend", "local", "-state-dir", state}
			assertCivoIngressError(t, run(append(args, test.args...)), test.want)
			assertCivoIngressPathAbsent(t, state)
		})
	}
	t.Run("other cloud", func(t *testing.T) {
		civoIngressTestHome(t)
		state := filepath.Join(t.TempDir(), "state")
		assertCivoIngressError(t, run([]string{"preview", "-cloud", "aws", "-state-dir", state, "-civo-ingress", "loadbalancer"}), "-civo-ingress and -civo-dns apply only to -cloud civo")
		assertCivoIngressPathAbsent(t, state)
	})
}

func TestRegisterDrainingRefusals(t *testing.T) {
	for _, test := range []struct {
		name string
		cmd  string
		args []string
		want string
	}{
		{"up needs control plane", "up", nil, "-register-draining requires -control-plane"},
		{"outputs needs control plane", "outputs", nil, "-register-draining requires -control-plane"},
		{"restore refuses draining", "up", []string{"-argocd", "-control-plane", "https://self.example.test", "-restore-archives"}, "-register-draining cannot be combined with -restore-archives"},
	} {
		t.Run(test.name, func(t *testing.T) {
			civoIngressTestHome(t)
			state := filepath.Join(t.TempDir(), "state")
			args := []string{test.cmd, "-cloud", "civo", "-region", "nyc1", "-backend", "local", "-state-dir", state, "-civo-admin-cidr", "203.0.113.7/32", "-register-draining"}
			assertCivoIngressError(t, run(append(args, test.args...)), test.want)
			assertCivoIngressPathAbsent(t, state)
		})
	}
}

func TestRequireCivoCloudflareEnv(t *testing.T) {
	for _, test := range []struct {
		name  string
		cmd   string
		cloud string
		dns   string
	}{
		{"other cloud", "preview", "aws", "cloudflare"},
		{"unmanaged DNS", "preview", "civo", "none"},
		{"outputs", "outputs", "civo", "cloudflare"},
		{"cell health", "cell-health", "civo", "cloudflare"},
		{"bootstrap", "bootstrap", "civo", "cloudflare"},
		{"state check", "state-check", "civo", "cloudflare"},
		{"whoami", "whoami", "civo", "cloudflare"},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(string) (string, bool) {
				t.Error("irrelevant operation inspected the Cloudflare environment")
				return "", false
			}
			assertCivoIngressError(t, requireCivoCloudflareEnv(test.cmd, test.cloud, test.dns, lookup), "")
		})
	}
	const fake = "test-cloudflare-token-not-real"
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"unset token", nil, civoMissingCloudflareMessage},
		{"empty token", map[string]string{"CLOUDFLARE_API_TOKEN": ""}, civoMissingCloudflareMessage},
		{"trailing newline", map[string]string{"CLOUDFLARE_API_TOKEN": fake + "\n"}, "civo_dns cloudflare: environment variable CLOUDFLARE_API_TOKEN has leading or trailing whitespace; export the exact value"},
		{"one ambient key", map[string]string{"CLOUDFLARE_API_TOKEN": fake, "CLOUDFLARE_API_KEY": "test-key-not-real"}, "civo_dns cloudflare: unset CLOUDFLARE_API_KEY in this shell; witself-infra uses CLOUDFLARE_API_TOKEN only, and the Cloudflare provider would read these as well"},
		{"empty ambient key", map[string]string{"CLOUDFLARE_API_TOKEN": fake, "CLOUDFLARE_API_KEY": ""}, "civo_dns cloudflare: unset CLOUDFLARE_API_KEY in this shell; witself-infra uses CLOUDFLARE_API_TOKEN only, and the Cloudflare provider would read these as well"},
		{
			name: "all ambient variables ordered",
			env: map[string]string{
				"CLOUDFLARE_API_TOKEN": fake, "CLOUDFLARE_API_KEY": "",
				"CLOUDFLARE_EMAIL": "", "CLOUDFLARE_API_USER_SERVICE_KEY": "",
				"CLOUDFLARE_BASE_URL": "",
			},
			want: "civo_dns cloudflare: unset CLOUDFLARE_API_KEY, CLOUDFLARE_EMAIL, CLOUDFLARE_API_USER_SERVICE_KEY, CLOUDFLARE_BASE_URL in this shell; witself-infra uses CLOUDFLARE_API_TOKEN only, and the Cloudflare provider would read these as well",
		},
		{"clean token", map[string]string{"CLOUDFLARE_API_TOKEN": fake}, ""},
	}
	for _, cmd := range []string{"up", "preview", "refresh", "destroy"} {
		t.Run(cmd, func(t *testing.T) {
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					lookup := func(name string) (string, bool) {
						value, ok := test.env[name]
						return value, ok
					}
					err := requireCivoCloudflareEnv(cmd, "civo", "cloudflare", lookup)
					if err != nil {
						for _, forbidden := range []string{fake, "test-key-not-real", "Zone Read", "DNS Edit"} {
							if strings.Contains(err.Error(), forbidden) {
								t.Fatal("environment refusal disclosed a value or named a permission")
							}
						}
					}
					assertCivoIngressError(t, err, test.want)
				})
			}
		})
	}
}

func TestCivoCloudflareTokenIsRequiredBeforeAnyWork(t *testing.T) {
	civoIngressTestHome(t)
	state := filepath.Join(t.TempDir(), "state")
	err := run([]string{
		"preview", "-cloud", "civo", "-region", "nyc1", "-backend", "local",
		"-state-dir", state, "-civo-ingress", "loadbalancer", "-civo-dns", "cloudflare",
	})
	assertCivoIngressError(t, err, civoMissingCloudflareMessage)
	assertCivoIngressPathAbsent(t, state)
}

func TestCivoStackConfig(t *testing.T) {
	base := map[string]string{
		"civo:region": "nyc1", "witself:civoNodeSize": "g4s.kube.medium",
		"witself:civoAdminCIDR": "203.0.113.7/32",
	}
	for _, ingress := range []string{"nodeport", ""} {
		name := ingress
		if name == "" {
			name = "empty ingress"
		}
		t.Run(name, func(t *testing.T) {
			for _, token := range []struct{ name, value string }{
				{"without environment token", ""}, {"with environment token", "test-cloudflare-token-not-real"},
			} {
				t.Run(token.name, func(t *testing.T) {
					t.Setenv("CLOUDFLARE_API_TOKEN", token.value)
					set, clearKeys := civoStackConfig("nyc1", "g4s.kube.medium", "203.0.113.7/32", ingress, "none", "cells.example.test")
					if !reflect.DeepEqual(set, base) {
						t.Error("default stack config differs from the original three provider keys")
					}
					if !reflect.DeepEqual(clearKeys, []string{"witself:cidr", "witself:dbVersion", "witself:domain", "witself:cloudflareDNS"}) {
						t.Error("default clear keys or their order changed")
					}
					if _, exists := set["witself:cloudflareDNS"]; exists {
						t.Error("Civo must never set the other clouds' DNS delegation key")
					}
				})
			}
		})
	}
	for _, dns := range []string{"none", "cloudflare"} {
		t.Run("loadbalancer/"+dns, func(t *testing.T) {
			set, clearKeys := civoStackConfig("nyc1", "g4s.kube.medium", "203.0.113.7/32", "loadbalancer", dns, "cells.example.test")
			want := map[string]string{
				"civo:region": "nyc1", "witself:civoNodeSize": "g4s.kube.medium",
				"witself:civoAdminCIDR": "203.0.113.7/32", "witself:civoIngress": "loadbalancer",
				"witself:civoDNS": dns, "witself:domain": "cells.example.test",
			}
			if !reflect.DeepEqual(set, want) {
				t.Error("load balancer stack config does not have exactly the six expected keys and values")
			}
			if !reflect.DeepEqual(clearKeys, []string{"witself:cidr", "witself:dbVersion", "witself:cloudflareDNS"}) {
				t.Error("load balancer clear keys changed or would clear the custom domain")
			}
			if _, exists := set["witself:cloudflareDNS"]; exists {
				t.Error("Civo must never set the other clouds' DNS delegation key")
			}
		})
	}
}

func TestFleetRegistrationDraining(t *testing.T) {
	for _, test := range []struct {
		name      string
		target    bool
		draining  bool
		accepting bool
	}{
		{"draining ordinary cell", false, true, false},
		{"draining restore target", true, true, false},
		{"ordinary cell open", false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cell := fleetRegistration(
				"civo-fixture-use1-serving", "api.example.test", "civo", "nyc1", "use1", "experimental",
				"witself_prv_provision-only", "witself_bak_backup-only", test.target, test.draining,
			)
			if cell.Accepting == nil || *cell.Accepting != test.accepting {
				t.Error("registration did not explicitly carry the expected accepting value")
			}
			if cell.BackupValidationTarget != test.target {
				t.Error("registration changed the backup validation target")
			}
		})
	}
}

func TestDrainedRegistrationReachesControlPlane(t *testing.T) {
	civoIngressTestHome(t)
	t.Setenv("WITSELF_FLEET_TOKEN", "test-fleet-token-not-real")
	body := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/cells" {
			t.Error("registration reached the wrong method or route")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error("could not read registration body")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body <- raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","cell":{"name":"civo-fixture-use1-serving","accepting":false,"backup_validation_target":false,"has_backup_token":true}}`))
	}))
	defer server.Close()
	client, err := fleet.NewClient(server.URL, "")
	assertCivoIngressError(t, err, "")
	err = client.Register(context.Background(), fleetRegistration(
		"civo-fixture-use1-serving", "api.example.test", "civo", "nyc1", "use1", "experimental",
		"witself_prv_provision-only", "witself_bak_backup-only", false, true,
	))
	assertCivoIngressError(t, err, "")
	select {
	case raw := <-body:
		if !strings.Contains(string(raw), `"accepting":false`) {
			t.Error("registration request omitted accepting=false")
		}
		if !strings.Contains(string(raw), `"backup_validation_target":false`) {
			t.Error("draining registration changed the restore target marker")
		}
	default:
		t.Fatal("the control plane received no registration request")
	}
}

func TestCivoIngressInventoryRoundTrip(t *testing.T) {
	civoIngressTestHome(t)
	path := filepath.Join(t.TempDir(), "infra.yaml")
	fs := newTestFlagSet()
	if err := fs.Parse([]string{
		"-cloud", "civo", "-account-alias", "fixture", "-region", "nyc1", "-role", "serving",
		"-profile", "prod", "-backend", "local", "-civo-admin-cidr", "203.0.113.7/32",
		"-civo-node-size", "g4s.kube.large", "-civo-ingress", "loadbalancer", "-civo-dns", "cloudflare",
		"-domain", "cells.example.test", "-register-draining", "-argocd", "-control-plane", "https://self.example.test",
	}); err != nil {
		t.Fatal(err)
	}
	assertCivoIngressError(t, configAddCell(fs, path), "")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"civo_ingress: loadbalancer", "civo_dns: cloudflare", "domain: cells.example.test", "register_draining: true"} {
		if !strings.Contains(string(raw), line) {
			t.Errorf("inventory omitted %q", line)
		}
	}
	cfg, _, err := loadInfraConfig(path)
	assertCivoIngressError(t, err, "")
	entry, ok := cfg.Cells["civo-fixture-use1-serving"]
	if !ok || entry.CivoIngress == nil || *entry.CivoIngress != "loadbalancer" ||
		entry.CivoDNS == nil || *entry.CivoDNS != "cloudflare" ||
		entry.Domain == nil || *entry.Domain != "cells.example.test" ||
		entry.RegisterDraining == nil || !*entry.RegisterDraining {
		t.Fatal("loader lost the per-cell ingress or registration settings")
	}
	resolved := newTestFlagSet()
	assertCivoIngressError(t, applyCellConfig(resolved, "civo-fixture-use1-serving", path), "")
	for name, want := range map[string]string{
		"civo-ingress": "loadbalancer", "civo-dns": "cloudflare", "domain": "cells.example.test", "register-draining": "true",
	} {
		if got := resolved.Lookup(name).Value.String(); got != want {
			t.Errorf("resolved flag %s does not equal %q", name, want)
		}
	}
}

func TestCivoIngressAddCellRefusals(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"domain without load balancer", []string{"-domain", "cells.example.test"}, "-domain does not apply to -cloud civo"},
		{"DNS without load balancer", []string{"-civo-dns", "cloudflare"}, "-civo-dns cloudflare requires -civo-ingress loadbalancer"},
		{"small node", []string{"-civo-node-size", "g4s.kube.small"}, `-civo-node-size "g4s.kube.small" is not supported for -cloud civo (want g4s.kube.medium or g4s.kube.large)`},
		{"draining without control plane", []string{"-register-draining"}, "-register-draining requires -control-plane"},
		{"other cloud", []string{"-cloud", "aws", "-region", "us-west-2", "-civo-ingress", "loadbalancer"}, "-civo-ingress and -civo-dns apply only to -cloud civo"},
	} {
		t.Run(test.name, func(t *testing.T) {
			civoIngressTestHome(t)
			path := filepath.Join(t.TempDir(), "infra.yaml")
			fs := newTestFlagSet()
			args := []string{"-cloud", "civo", "-region", "nyc1", "-backend", "local", "-civo-admin-cidr", "203.0.113.7/32"}
			if err := fs.Parse(append(args, test.args...)); err != nil {
				t.Fatal(err)
			}
			assertCivoIngressError(t, configAddCell(fs, path), test.want)
			assertCivoIngressPathAbsent(t, path)
		})
	}
}

func TestCivoIngressLoaderRefusals(t *testing.T) {
	for _, test := range []struct{ name, field string }{
		{"ingress default", "civo_ingress: nodeport"},
		{"DNS default", "civo_dns: none"},
		{"registration default", "register_draining: false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, "version: 1\ndefaults:\n  "+test.field+"\ncells: {}\n")
			_, _, err := loadInfraConfig(path)
			assertCivoIngressError(t, err, path+": defaults must not set civo_ingress/civo_dns/register_draining — ingress shape and registration state are per-cell only")
		})
	}
	for _, test := range []struct {
		name   string
		cloud  string
		fields string
		want   string
	}{
		{"ingress on other cloud", "aws", "civo_ingress: nodeport", "civo_ingress and civo_dns apply only to cloud civo"},
		{"DNS on other cloud", "aws", "civo_dns: none", "civo_ingress and civo_dns apply only to cloud civo"},
		{"unknown ingress", "civo", "civo_ingress: other", "civo_ingress must be nodeport or loadbalancer"},
		{"unknown DNS", "civo", "civo_dns: other", "civo_dns must be none or cloudflare"},
		{"DNS without load balancer", "civo", "civo_dns: cloudflare", "civo_dns cloudflare requires civo_ingress loadbalancer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, "version: 1\ncells:\n  fixture:\n    cloud: "+test.cloud+"\n    "+test.fields+"\n")
			_, _, err := loadInfraConfig(path)
			assertCivoIngressError(t, err, path+`: cell "fixture": `+test.want)
		})
	}
}

func TestCivoIngressSettingsRows(t *testing.T) {
	cloud, ingress, dns, domain, draining := "civo", "loadbalancer", "cloudflare", "cells.example.test", true
	none := "none"
	tests := []struct {
		name  string
		entry cellEntry
		want  []settingRow
	}{
		{
			name: "managed DNS and draining",
			entry: cellEntry{
				Cloud: &cloud, CivoIngress: &ingress, CivoDNS: &dns, Domain: &domain, RegisterDraining: &draining,
			},
			want: []settingRow{
				{key: "registration", value: "draining (accepting=false)", fromEntry: true},
				{key: "ingress", value: "Civo load balancer · Traefik", fromEntry: true},
				{key: "dns record", value: "Cloudflare · managed by Pulumi", fromEntry: true},
				{key: "domain", value: "cells.example.test", fromEntry: true},
			},
		},
		{
			name:  "operator DNS by default",
			entry: cellEntry{Cloud: &cloud, CivoIngress: &ingress},
			want: []settingRow{
				{key: "ingress", value: "Civo load balancer · Traefik", fromEntry: true},
				{key: "dns record", value: "operator-managed", fromEntry: false},
				{key: "domain", value: "cells.witself.witwave.ai", fromEntry: false},
			},
		},
		{
			name:  "original nodeport",
			entry: cellEntry{Cloud: &cloud},
			want:  []settingRow{{key: "ingress", value: "Civo DNS · Traefik NodePort", fromEntry: false}},
		},
		{
			name:  "explicit operator DNS",
			entry: cellEntry{Cloud: &cloud, CivoIngress: &ingress, CivoDNS: &none},
			want: []settingRow{
				{key: "ingress", value: "Civo load balancer · Traefik", fromEntry: true},
				{key: "dns record", value: "operator-managed", fromEntry: true},
				{key: "domain", value: "cells.witself.witwave.ai", fromEntry: false},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got []settingRow
			for _, row := range effectiveSettings(test.entry, nil) {
				switch row.key {
				case "registration", "ingress", "dns record", "domain":
					got = append(got, row)
				}
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("ingress settings rows = %#v, want %#v", got, test.want)
			}
		})
	}
}

func m1(cell, cmd, recorded, target string) string {
	return fmt.Sprintf("cell %s has the recorded apiHost %q, and this %s would change it to %s. A cell that may be registered with a control plane keeps its host: restore this cell's previous civo_ingress, domain and argocd settings, or provision a new cell with the new settings, move its accounts there (witself-admin cells evacuate, then cells restore) and destroy this cell once it is empty", cell, recorded, cmd, target)
}

func m2(cell, cmd, recorded string) string {
	return fmt.Sprintf("cell %s has the recorded apiHost %q but no recorded civoDNSEntry, so this %s cannot show that the host stays the same. A cell that may be registered with a control plane keeps its host: provision a new cell, move its accounts there (witself-admin cells evacuate, then cells restore) and destroy this cell once it is empty", cell, recorded, cmd)
}

func m3(cell, cmd string) string {
	return fmt.Sprintf("cell %s has a recorded apiHost that is not a string, so this %s cannot show that the host stays the same. A cell that may be registered with a control plane keeps its host: provision a new cell, move its accounts there (witself-admin cells evacuate, then cells restore) and destroy this cell once it is empty", cell, cmd)
}

func TestCheckCivoAPIHost(t *testing.T) {
	const cell = "civo-example-use1-dev"
	const domain = "cells.example.test"
	const npHost = "api.test-cluster-id.k8s.civo.com"
	const lbHost = "api.civo-example-use1-dev.cells.example.test"
	const provisionToken = "test-provision-token-not-real"
	np := auto.OutputMap{
		"apiHost":      {Value: npHost},
		"civoDNSEntry": {Value: "test-cluster-id.k8s.civo.com"},
	}
	lb := auto.OutputMap{
		"apiHost":      {Value: lbHost},
		"civoDNSEntry": {Value: "test-cluster-id.k8s.civo.com"},
	}
	for _, test := range []struct {
		name, cmd, ingress, domain string
		argocd                     bool
		outs                       auto.OutputMap
		want                       string
	}{
		{"1", "preview", "loadbalancer", domain, false, nil, ""},
		{"2", "up", "nodeport", "", true, nil, ""},
		{"3", "up", "nodeport", "", true, auto.OutputMap{"apiHost": {Value: ""}}, ""},
		{"4", "up", "nodeport", "", true, auto.OutputMap{"apiHost": {Value: 42.0}}, m3(cell, "up")},
		{"5", "up", "nodeport", "", true, np, ""},
		{"6", "preview", "loadbalancer", domain, true, np, m1(cell, "preview", npHost, "\""+lbHost+"\"")},
		{"7", "up", "loadbalancer", domain, true, np, m1(cell, "up", npHost, "\""+lbHost+"\"")},
		{"8", "up", "loadbalancer", domain, true, lb, ""},
		{"9", "preview", "loadbalancer", domain, false, lb, ""},
		{"10", "up", "loadbalancer", " Cells.Example.Test. ", false, lb, ""},
		{"11", "up", "loadbalancer", "cells.other.test", false, lb, m1(cell, "up", lbHost, "\"api.civo-example-use1-dev.cells.other.test\"")},
		{"12", "up", "nodeport", "", true, lb, m1(cell, "up", lbHost, "\""+npHost+"\"")},
		{"13", "up", "nodeport", "", false, lb, m1(cell, "up", lbHost, "no host")},
		{"14", "preview", "nodeport", "", false, np, m1(cell, "preview", npHost, "no host")},
		{"15", "up", "nodeport", "", true, auto.OutputMap{"apiHost": {Value: npHost}}, m2(cell, "up", npHost)},
		{"16", "preview", "nodeport", "", true, auto.OutputMap{"apiHost": {Value: npHost}, "civoDNSEntry": {Value: ""}}, m2(cell, "preview", npHost)},
		{"17", "up", "loadbalancer", domain, false, auto.OutputMap{"apiHost": {Value: npHost}}, m1(cell, "up", npHost, "\""+lbHost+"\"")},
	} {
		t.Run(test.name, func(t *testing.T) {
			outs := auto.OutputMap{"provisionToken": {Value: provisionToken, Secret: true}}
			for name, output := range test.outs {
				outs[name] = output
			}
			err := checkCivoAPIHost(test.cmd, cell, test.ingress, test.domain, test.argocd, outs)
			if err != nil && strings.Contains(err.Error(), provisionToken) {
				t.Fatal("row disclosed a secret output")
			}
			if test.want == "" {
				if err != nil {
					t.Fatal("row unexpectedly refused")
				}
			} else if err == nil || err.Error() != test.want {
				t.Fatal("row refusal message differs")
			}
		})
	}
}

func TestRefuseCivoAPIHostChangeReadFailure(t *testing.T) {
	cause := errors.New("fake read failure")
	calls := 0
	reader := func(context.Context) (auto.OutputMap, error) {
		calls++
		return nil, cause
	}
	err := refuseCivoAPIHostChange(context.Background(), reader, "up", "civo-example-use1-dev", "loadbalancer", "cells.example.test", false)
	if err == nil || err.Error() != "read the recorded outputs of cell civo-example-use1-dev before up: fake read failure" {
		t.Fatal("read failure message differs")
	}
	if !errors.Is(err, cause) || calls != 1 {
		t.Fatal("read failure cause or reader call count differs")
	}
	calls = 0
	reader = func(context.Context) (auto.OutputMap, error) {
		calls++
		return auto.OutputMap{
			"apiHost":      {Value: "api.civo-example-use1-dev.cells.example.test"},
			"civoDNSEntry": {Value: "test-cluster-id.k8s.civo.com"},
		}, nil
	}
	err = refuseCivoAPIHostChange(context.Background(), reader, "up", "civo-example-use1-dev", "loadbalancer", "cells.example.test", false)
	if err != nil || calls != 1 {
		t.Fatal("unchanged host result or reader call count differs")
	}
}

func TestCivoAPIHostGuardRunsBeforeAnyStackChange(t *testing.T) {
	const npJSON = `{"apiHost":"api.test-cluster-id.k8s.civo.com","civoDNSEntry":"test-cluster-id.k8s.civo.com"}`
	const lbJSON = `{"apiHost":"api.civo-spike-use1-dev.cells.example.test","civoDNSEntry":"test-cluster-id.k8s.civo.com"}`
	for _, test := range []struct {
		name, cmd, outputJSON string
		args                  []string
		missing, stop         bool
		want                  string
	}{
		{
			name: "G1", cmd: "preview", outputJSON: npJSON,
			args: []string{"-civo-ingress", "loadbalancer", "-domain", "cells.example.test"},
			want: m1("civo-spike-use1-dev", "preview", "api.test-cluster-id.k8s.civo.com", "\"api.civo-spike-use1-dev.cells.example.test\""),
		},
		{
			name: "G2", cmd: "up", outputJSON: npJSON,
			args: []string{"-civo-ingress", "loadbalancer", "-domain", "cells.example.test"},
			want: m1("civo-spike-use1-dev", "up", "api.test-cluster-id.k8s.civo.com", "\"api.civo-spike-use1-dev.cells.example.test\""),
		},
		{
			name: "G3", cmd: "preview", outputJSON: npJSON,
			args: []string{"-argocd"}, stop: true,
		},
		{
			name: "G4", cmd: "up", outputJSON: lbJSON,
			args: []string{"-civo-ingress", "loadbalancer", "-domain", "Cells.Example.Test."}, stop: true,
		},
		{
			name: "G5", cmd: "preview", outputJSON: "{}",
			args: []string{"-civo-ingress", "loadbalancer", "-domain", "cells.example.test"}, missing: true, stop: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			r2TestHome(t)
			for name, value := range r2TestValues() {
				t.Setenv(name, value)
			}
			t.Setenv("CIVO_TOKEN", "test-civo-token-not-real")
			t.Setenv("TMPDIR", t.TempDir())
			r2StubCheck(t, func(context.Context, backend.R2Settings, backend.R2Secrets, backend.R2CheckOptions) (backend.R2CheckReport, error) {
				return backend.R2CheckReport{}, nil
			})
			stopErr := errors.New("fake config stop")
			f := &r2FakePulumi{t: t, missing: test.missing, outputJSON: test.outputJSON}
			if test.stop {
				f.stopErr = stopErr
			}
			r2StubCLICommand(t, f)
			args := []string{test.cmd, "-cloud", "civo", "-account-alias", "spike", "-region", "nyc1", "-role", "dev", "-backend", "r2", "-r2-bucket", r2TestSettings.Bucket, "-r2-endpoint", r2TestEndpoint, "-civo-admin-cidr", "203.0.113.7/32"}
			err := run(append(args, test.args...))
			wantCalls := [][]string{{"stack", "select", "--stack", "civo-spike-use1-dev"}}
			if test.missing {
				wantCalls = append(wantCalls, []string{"stack", "init", "civo-spike-use1-dev"})
			}
			wantCalls = append(wantCalls,
				[]string{"stack", "output", "--json", "--stack", "civo-spike-use1-dev"},
				[]string{"stack", "output", "--json", "--show-secrets", "--stack", "civo-spike-use1-dev"})
			if !test.stop {
				r2TestError(t, err, test.want)
				if !reflect.DeepEqual(f.calls, wantCalls) {
					t.Fatal("row refusal command sequence differs")
				}
				return
			}
			if !errors.Is(err, stopErr) || !strings.HasPrefix(err.Error(), "set config ") {
				t.Fatal("row did not reach the config stop")
			}
			if len(f.calls) != len(wantCalls)+1 || !reflect.DeepEqual(f.calls[:len(wantCalls)], wantCalls) {
				t.Fatal("row accepted command sequence differs")
			}
			last := f.calls[len(wantCalls)]
			if len(last) < 2 || last[0] != "config" || last[1] != "set" {
				t.Fatal("row first setting command differs")
			}
		})
	}
}
