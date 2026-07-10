package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"gopkg.in/yaml.v3"
)

// errLocalAuth marks a failure to mint LOCAL cloud credentials
// (expired gcloud ADC, a dead `az` session). Unlike a cluster that's
// still converging, polling cannot heal this — the fix is a human
// running a login flow — so the convergence wait aborts immediately
// instead of retrying the same doomed call until maxWait.
var errLocalAuth = errors.New("local cloud credentials unavailable")

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
	if !argocd {
		return nil
	}
	switch cloud {
	case "gcp":
		return waitForGCPArgoApplicationsHealthy(ctx, stack, maxWait, pollEvery)
	case "azure":
		return waitForAzureArgoApplicationsHealthy(ctx, stack, maxWait, pollEvery)
	default:
		return nil
	}
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

func waitForAzureArgoApplicationsHealthy(ctx context.Context, stack auto.Stack, maxWait, pollEvery time.Duration) error {
	outs, err := stack.Outputs(ctx)
	if err != nil {
		return fmt.Errorf("read outputs for GitOps verification: %w", err)
	}
	lister, namespace, err := newAzureArgoListerFromOutputs(ctx, outs)
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
		// A local-credential failure can't heal by polling — every retry
		// runs the same login-required CLI on the same machine. Abort
		// with the remedy up front instead of burning maxWait on it.
		if err != nil && errors.Is(err, errLocalAuth) {
			return fmt.Errorf("argo CD verification aborted after %s: %w — run the login flow (dashboard: press `a`) and re-run the op", time.Since(started).Round(time.Second), err)
		}
		var reason string
		if err == nil {
			ready, why := argoApplicationsReady(apps)
			if ready {
				fmt.Fprintf(os.Stderr, "Argo CD applications Synced/Healthy (took %s)\n", time.Since(started).Round(time.Second))
				return nil
			}
			reason = why
		} else {
			reason = err.Error()
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("argo CD applications in %s did not become Synced/Healthy within %s (last: %s)", namespace, maxWait, reason)
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

type tokenArgoLister struct {
	baseURL string
	client  *http.Client
	token   string
}

func newAzureArgoListerFromOutputs(ctx context.Context, outs auto.OutputMap) (*tokenArgoLister, string, error) {
	resourceGroup := outputString(outs, "resourceGroup")
	if resourceGroup == "" {
		return nil, "", fmt.Errorf("stack exports no resourceGroup; cannot verify Argo CD health")
	}
	clusterName := outputString(outs, "aksCluster")
	if clusterName == "" {
		return nil, "", fmt.Errorf("stack exports no aksCluster; cannot verify Argo CD health")
	}
	namespace := outputString(outs, "argocdNamespace")
	if namespace == "" {
		namespace = "argocd"
	}
	raw, err := azureAKSKubeconfig(ctx, resourceGroup, clusterName)
	if err != nil {
		return nil, "", err
	}
	lister, err := newAzureArgoListerFromKubeconfig(raw)
	if err != nil {
		return nil, "", err
	}
	return lister, namespace, nil
}

func newAzureArgoListerFromKubeconfig(raw []byte) (*tokenArgoLister, error) {
	var cfg kubeconfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("decode AKS kubeconfig: %w", err)
	}
	if len(cfg.Clusters) == 0 {
		return nil, fmt.Errorf("AKS kubeconfig contained no clusters")
	}
	server := strings.TrimSpace(cfg.Clusters[0].Cluster.Server)
	if server == "" {
		return nil, fmt.Errorf("AKS kubeconfig cluster contained no server")
	}
	caData := strings.TrimSpace(cfg.Clusters[0].Cluster.CertificateAuthorityData)
	if caData == "" {
		return nil, fmt.Errorf("AKS kubeconfig cluster contained no certificate-authority-data")
	}
	ca, err := base64.StdEncoding.DecodeString(caData)
	if err != nil {
		return nil, fmt.Errorf("decode AKS certificate authority: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("AKS certificate authority output did not contain PEM data")
	}
	token := ""
	for _, user := range cfg.Users {
		token = strings.TrimSpace(user.User.Token)
		if token != "" {
			break
		}
	}
	if token == "" {
		return nil, fmt.Errorf("AKS kubeconfig contained no bearer token")
	}
	return &tokenArgoLister{
		baseURL: strings.TrimRight(server, "/"),
		client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		}},
		token: token,
	}, nil
}

func azureAKSKubeconfig(ctx context.Context, resourceGroup, clusterName string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "az", "aks", "get-credentials",
		"--resource-group", resourceGroup,
		"--name", clusterName,
		"--format", "exec",
		"--file", "-",
		"--overwrite-existing",
		"--only-show-errors",
	)
	out, err := cmd.Output()
	if err != nil {
		detail := ""
		if ee, ok := err.(*exec.ExitError); ok {
			detail = strings.TrimSpace(string(ee.Stderr))
		}
		if detail != "" {
			return nil, fmt.Errorf("%w: get AKS credentials: %v: %s", errLocalAuth, err, detail)
		}
		return nil, fmt.Errorf("%w: get AKS credentials: %v", errLocalAuth, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("get AKS credentials: command returned empty kubeconfig")
	}
	return out, nil
}

func (l *tokenArgoLister) ListArgoApplications(ctx context.Context, namespace string) ([]argoApplication, error) {
	url := fmt.Sprintf("%s/apis/argoproj.io/v1alpha1/namespaces/%s/applications", l.baseURL, namespace)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+l.token)
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

type kubeconfig struct {
	Clusters []struct {
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		User struct {
			Token string `yaml:"token"`
		} `yaml:"user"`
	} `yaml:"users"`
}

func gcpADCAccessToken(ctx context.Context, project string) (string, error) {
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "print-access-token")
	if project != "" {
		cmd.Env = append(os.Environ(), "CLOUDSDK_CORE_PROJECT="+project)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: mint GCP ADC access token: %v: %s", errLocalAuth, err, strings.TrimSpace(string(out)))
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("%w: mint GCP ADC access token: command returned an empty token", errLocalAuth)
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
