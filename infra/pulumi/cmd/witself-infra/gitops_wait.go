package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

type argoApplication struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Sync struct {
			Status string `json:"status"`
		} `json:"sync"`
		Health struct {
			Status  string `json:"status"`
			Message string `json:"message,omitempty"`
		} `json:"health"`
		Conditions []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"conditions,omitempty"`
	} `json:"status"`
}

type argoApplicationList struct {
	Items []argoApplication `json:"items"`
}

type argoApplicationLister interface {
	ListArgoApplications(ctx context.Context, namespace string) ([]argoApplication, error)
}

func waitForPostUpConvergence(ctx context.Context, stack auto.Stack, cloud string, argocd bool, maxWait, pollEvery time.Duration) error {
	if !argocd || cloud != "gcp" {
		return nil
	}
	return waitForGCPArgoApplicationsHealthy(ctx, stack, maxWait, pollEvery)
}

func waitForGCPArgoApplicationsHealthy(ctx context.Context, stack auto.Stack, maxWait, pollEvery time.Duration) error {
	outs, err := stack.Outputs(ctx)
	if err != nil {
		return fmt.Errorf("read outputs for GitOps verification: %w", err)
	}
	lister, namespace, err := newGCPArgoListerFromOutputs(outs)
	if err != nil {
		return err
	}
	return waitForArgoApplicationsHealthy(ctx, lister, namespace, maxWait, pollEvery)
}

func waitForArgoApplicationsHealthy(ctx context.Context, lister argoApplicationLister, namespace string, maxWait, pollEvery time.Duration) error {
	deadline := time.Now().Add(maxWait)
	started := time.Now()

	fmt.Fprintf(os.Stderr, "waiting for Argo CD applications in %s to be Synced/Healthy (up to %s)...\n", namespace, maxWait)

	for {
		apps, err := lister.ListArgoApplications(ctx, namespace)
		var reason string
		if err == nil {
			if ready, why := argoApplicationsReady(apps); ready {
				fmt.Fprintf(os.Stderr, "Argo CD applications Synced/Healthy (took %s)\n", time.Since(started).Round(time.Second))
				return nil
			} else {
				reason = why
			}
		} else {
			reason = err.Error()
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("Argo CD applications in %s did not become Synced/Healthy within %s (last: %s)", namespace, maxWait, reason)
		}

		fmt.Fprintf(os.Stderr, "  Argo CD: %s (%s elapsed)\n", truncate(reason, 160), time.Since(started).Round(time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

func argoApplicationsReady(apps []argoApplication) (bool, string) {
	if len(apps) == 0 {
		return false, "no Argo CD applications reported yet"
	}
	var pending []string
	for _, app := range apps {
		name := app.Metadata.Name
		if name == "" {
			name = "<unknown>"
		}
		sync := app.Status.Sync.Status
		health := app.Status.Health.Status
		if sync == "Synced" && health == "Healthy" {
			continue
		}
		if sync == "" {
			sync = "Unknown"
		}
		if health == "" {
			health = "Unknown"
		}
		msg := fmt.Sprintf("%s %s/%s", name, sync, health)
		if app.Status.Health.Message != "" {
			msg += ": " + app.Status.Health.Message
		}
		for _, cond := range app.Status.Conditions {
			if cond.Message == "" {
				continue
			}
			msg += ": " + cond.Message
			break
		}
		pending = append(pending, msg)
	}
	if len(pending) == 0 {
		return true, ""
	}
	return false, strings.Join(pending, "; ")
}

type gcpArgoLister struct {
	project  string
	baseURL  string
	client   *http.Client
	tokenCmd func(context.Context, string) (string, error)
}

func newGCPArgoListerFromOutputs(outs auto.OutputMap) (*gcpArgoLister, string, error) {
	endpoint := outputString(outs, "gkeEndpoint")
	if endpoint == "" {
		return nil, "", fmt.Errorf("stack exports no gkeEndpoint; cannot verify Argo CD health")
	}
	caData := outputString(outs, "gkeCertificateAuthority")
	if caData == "" {
		return nil, "", fmt.Errorf("stack exports no gkeCertificateAuthority; cannot verify Argo CD health")
	}
	ca, err := base64.StdEncoding.DecodeString(caData)
	if err != nil {
		return nil, "", fmt.Errorf("decode GKE certificate authority: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, "", fmt.Errorf("GKE certificate authority output did not contain PEM data")
	}
	namespace := outputString(outs, "argocdNamespace")
	if namespace == "" {
		namespace = "argocd"
	}
	baseURL := endpoint
	if !strings.HasPrefix(baseURL, "https://") {
		baseURL = "https://" + baseURL
	}
	return &gcpArgoLister{
		project: outputString(outs, "gcpProject"),
		baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		}},
		tokenCmd: gcpADCAccessToken,
	}, namespace, nil
}

func (l *gcpArgoLister) ListArgoApplications(ctx context.Context, namespace string) ([]argoApplication, error) {
	token, err := l.tokenCmd(ctx, l.project)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/apis/argoproj.io/v1alpha1/namespaces/%s/applications", l.baseURL, namespace)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query Argo CD applications: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query Argo CD applications: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out argoApplicationList
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode Argo CD applications: %w", err)
	}
	return out.Items, nil
}

func gcpADCAccessToken(ctx context.Context, project string) (string, error) {
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "print-access-token")
	if project != "" {
		cmd.Env = append(os.Environ(), "CLOUDSDK_CORE_PROJECT="+project)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("mint GCP ADC access token: %w: %s", err, strings.TrimSpace(string(out)))
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("mint GCP ADC access token: command returned an empty token")
	}
	return token, nil
}

func outputString(outs auto.OutputMap, name string) string {
	out, ok := outs[name]
	if !ok {
		return ""
	}
	if s, ok := out.Value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
