package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

var civoNodeSizes = map[string]bool{"g4s.kube.medium": true, "g4s.kube.large": true}

type civoIngressOptions struct {
	Ingress  string
	DNS      string
	Domain   string
	NodeSize string
	CellName string
}

func validateCivoIngress(cmd string, o civoIngressOptions) error {
	if o.Ingress != "nodeport" && o.Ingress != "loadbalancer" {
		return fmt.Errorf("unknown -civo-ingress %q (want nodeport|loadbalancer)", o.Ingress)
	}
	if o.DNS != "none" && o.DNS != "cloudflare" {
		return fmt.Errorf("unknown -civo-dns %q (want none|cloudflare)", o.DNS)
	}
	if o.DNS == "cloudflare" && o.Ingress != "loadbalancer" {
		return fmt.Errorf("-civo-dns cloudflare requires -civo-ingress loadbalancer")
	}
	if cmd != "up" && cmd != "preview" && cmd != "add-cell" {
		return nil
	}
	if o.Ingress == "loadbalancer" && o.Domain == "" {
		return fmt.Errorf("-civo-ingress loadbalancer requires -domain, the parent domain of the host api.<cell>.<domain>")
	}
	if o.Ingress == "loadbalancer" && len(o.CellName) > 63 {
		return fmt.Errorf("cell name %q is longer than 63 characters and cannot be a label of the host api.<cell>.<domain>", o.CellName)
	}
	if !civoNodeSizes[o.NodeSize] {
		return fmt.Errorf("-civo-node-size %q is not supported for -cloud civo (want g4s.kube.medium or g4s.kube.large)", o.NodeSize)
	}
	return nil
}

func requireCivoCloudflareEnv(cmd, cloud, dns string, lookupenv func(string) (string, bool)) error {
	if cloud != "civo" || dns != "cloudflare" {
		return nil
	}
	switch cmd {
	case "up", "preview", "refresh", "destroy":
	default:
		return nil
	}
	token, ok := lookupenv("CLOUDFLARE_API_TOKEN")
	if !ok || token == "" {
		return fmt.Errorf("civo_dns cloudflare: missing environment variable CLOUDFLARE_API_TOKEN; export it in the shell that runs witself-infra (see the README for the token's permissions). It is never read from infra.yaml, a flag or a file")
	}
	if token != strings.TrimSpace(token) {
		return fmt.Errorf("civo_dns cloudflare: environment variable CLOUDFLARE_API_TOKEN has leading or trailing whitespace; export the exact value")
	}
	var ambient []string
	for _, name := range []string{"CLOUDFLARE_API_KEY", "CLOUDFLARE_EMAIL", "CLOUDFLARE_API_USER_SERVICE_KEY", "CLOUDFLARE_BASE_URL"} {
		if _, set := lookupenv(name); set {
			ambient = append(ambient, name)
		}
	}
	if len(ambient) != 0 {
		return fmt.Errorf("civo_dns cloudflare: unset %s in this shell; witself-infra uses CLOUDFLARE_API_TOKEN only, and the Cloudflare provider would read these as well", strings.Join(ambient, ", "))
	}
	return nil
}

func civoStackConfig(region, nodeSize, adminCIDR, ingress, dns, domain string) (set map[string]string, clearKeys []string) {
	set = map[string]string{
		"civo:region":           region,
		"witself:civoNodeSize":  nodeSize,
		"witself:civoAdminCIDR": adminCIDR,
	}
	if ingress != "loadbalancer" {
		return set, []string{"witself:cidr", "witself:dbVersion", "witself:domain", "witself:cloudflareDNS"}
	}
	set["witself:civoIngress"] = "loadbalancer"
	set["witself:civoDNS"] = dns
	set["witself:domain"] = domain
	return set, []string{"witself:cidr", "witself:dbVersion", "witself:cloudflareDNS"}
}

// refuseCivoAPIHostChange reads the stack's recorded outputs once and refuses
// a preview or up whose program would serve a different apiHost. It runs
// before any stack setting is written, so a refused run changes nothing. It
// reads no control plane: a cell with a recorded host may be registered.
func refuseCivoAPIHostChange(ctx context.Context, outputs stackOutputsReader, cmd, cellName, ingress, domain string, argocd bool) error {
	outs, err := outputs(ctx)
	if err != nil {
		return fmt.Errorf("read the recorded outputs of cell %s before %s: %w", cellName, cmd, err)
	}
	return checkCivoAPIHost(cmd, cellName, ingress, domain, argocd, outs)
}

// checkCivoAPIHost predicts the host the Civo program serves (see
// internal/cell/civo.go) and compares it with the recorded apiHost. A missing
// or empty recorded apiHost means the cell was never registered; a recorded
// apiHost that is not a string is refused.
func checkCivoAPIHost(cmd, cellName, ingress, domain string, argocd bool, outs auto.OutputMap) error {
	raw, present := outs["apiHost"]
	recorded, isString := raw.Value.(string)
	if present && !isString {
		return fmt.Errorf("cell %s has a recorded apiHost that is not a string, so this %s cannot show that the host stays the same. A cell that may be registered with a control plane keeps its host: provision a new cell, move its accounts there (witself-admin cells evacuate, then cells restore) and destroy this cell once it is empty", cellName, cmd)
	}
	if recorded == "" {
		return nil
	}
	next := ""
	switch {
	case ingress == "loadbalancer":
		next = "api." + cellName + "." + strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	case argocd:
		entry, _ := outs["civoDNSEntry"].Value.(string)
		if entry == "" {
			return fmt.Errorf("cell %s has the recorded apiHost %q but no recorded civoDNSEntry, so this %s cannot show that the host stays the same. A cell that may be registered with a control plane keeps its host: provision a new cell, move its accounts there (witself-admin cells evacuate, then cells restore) and destroy this cell once it is empty", cellName, recorded, cmd)
		}
		next = "api." + entry
	}
	if next == recorded {
		return nil
	}
	target := "no host"
	if next != "" {
		target = fmt.Sprintf("%q", next)
	}
	return fmt.Errorf("cell %s has the recorded apiHost %q, and this %s would change it to %s. A cell that may be registered with a control plane keeps its host: restore this cell's previous civo_ingress, domain and argocd settings, or provision a new cell with the new settings, move its accounts there (witself-admin cells evacuate, then cells restore) and destroy this cell once it is empty", cellName, recorded, cmd, target)
}
