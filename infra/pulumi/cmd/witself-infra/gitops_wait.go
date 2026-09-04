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
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"gopkg.in/yaml.v3"
)

// errLocalAuth marks a failure to mint LOCAL cloud credentials
// (expired gcloud ADC, a dead `az` session). Unlike a cluster that's
// still converging, polling cannot heal this — the fix is a human
// running a login flow — so the convergence wait aborts immediately
// instead of retrying the same doomed call until maxWait.
var errLocalAuth = errors.New("local cloud credentials unavailable")

const awsEKSTokenRefreshBefore = time.Minute

type argoApplicationSource struct {
	TargetRevision string `json:"targetRevision"`
}

type argoApplication struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Source  argoApplicationSource   `json:"source"`
		Sources []argoApplicationSource `json:"sources"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status     string `json:"status"`
			ComparedTo struct {
				Source  argoApplicationSource   `json:"source"`
				Sources []argoApplicationSource `json:"sources"`
			} `json:"comparedTo"`
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

type argoApplicationExpectation struct {
	name           string
	targetRevision string
}

// expectedBootstrapArgoApplications is the explicit contract for the managed
// bootstrap chart: Pulumi creates the bootstrap root, and the chart declares
// the unconditional apps and platform child Applications in
// .gitops/charts/bootstrap/templates. Nested Applications are intentionally not
// included because their target revisions are Helm chart versions.
func expectedBootstrapArgoApplications(targetRevision string) []argoApplicationExpectation {
	return []argoApplicationExpectation{
		{name: "bootstrap", targetRevision: targetRevision},
		{name: "apps", targetRevision: targetRevision},
		{name: "platform", targetRevision: targetRevision},
	}
}

func waitForPostUpConvergence(ctx context.Context, stack auto.Stack, cloud, region, awsProfile string, expected []argoApplicationExpectation, argocd bool, maxWait, pollEvery time.Duration) error {
	if !argocd {
		return nil
	}
	switch cloud {
	case "aws":
		return waitForAWSArgoApplicationsHealthy(ctx, stack, region, awsProfile, expected, maxWait, pollEvery)
	case "gcp":
		return waitForGCPArgoApplicationsHealthy(ctx, stack, expected, maxWait, pollEvery)
	case "azure":
		return waitForAzureArgoApplicationsHealthy(ctx, stack, expected, maxWait, pollEvery)
	case "civo":
		return waitForCivoArgoApplicationsHealthy(ctx, stack, expected, maxWait, pollEvery)
	default:
		return nil
	}
}

type stackOutputsReader func(context.Context) (auto.OutputMap, error)

type awsArgoListerFactory func(context.Context, auto.OutputMap, string, string) (argoApplicationLister, string, error)

func waitForAWSArgoApplicationsHealthy(ctx context.Context, stack auto.Stack, region, profile string, expected []argoApplicationExpectation, maxWait, pollEvery time.Duration) error {
	return waitForAWSArgoApplicationsHealthyWith(
		ctx,
		stack.Outputs,
		func(ctx context.Context, outs auto.OutputMap, region, profile string) (argoApplicationLister, string, error) {
			return newAWSArgoListerFromOutputs(ctx, outs, region, profile)
		},
		region,
		profile,
		expected,
		maxWait,
		pollEvery,
	)
}

// waitForAWSArgoApplicationsHealthyWith keeps the post-up wiring testable
// without constructing an Automation API stack or contacting AWS. Production
// supplies stack.Outputs and newAWSArgoListerFromOutputs above.
func waitForAWSArgoApplicationsHealthyWith(ctx context.Context, outputs stackOutputsReader, newLister awsArgoListerFactory, region, profile string, expected []argoApplicationExpectation, maxWait, pollEvery time.Duration) error {
	outs, err := outputs(ctx)
	if err != nil {
		return fmt.Errorf("read outputs for GitOps verification: %w", err)
	}
	lister, namespace, err := newLister(ctx, outs, region, profile)
	if err != nil {
		return err
	}
	return waitForArgoApplicationsHealthy(ctx, lister, namespace, expected, maxWait, pollEvery)
}

func waitForCivoArgoApplicationsHealthy(ctx context.Context, stack auto.Stack, expected []argoApplicationExpectation, maxWait, pollEvery time.Duration) error {
	outs, err := stack.Outputs(ctx)
	if err != nil {
		return fmt.Errorf("read outputs for GitOps verification: %w", err)
	}
	lister, namespace, err := newCivoArgoListerFromOutputs(outs)
	if err != nil {
		return err
	}
	return waitForArgoApplicationsHealthy(ctx, lister, namespace, expected, maxWait, pollEvery)
}

func newCivoArgoListerFromOutputs(outs auto.OutputMap) (*tokenArgoLister, string, error) {
	raw := outputString(outs, "kubeconfig")
	if raw == "" {
		return nil, "", fmt.Errorf("stack exports no Civo kubeconfig; cannot verify Argo CD health")
	}
	lister, err := newAzureArgoListerFromKubeconfig([]byte(raw))
	if err != nil {
		return nil, "", fmt.Errorf("build Civo cluster client: %w", err)
	}
	namespace := outputString(outs, "argocdNamespace")
	if namespace == "" {
		namespace = "argocd"
	}
	return lister, namespace, nil
}

func waitForGCPArgoApplicationsHealthy(ctx context.Context, stack auto.Stack, expected []argoApplicationExpectation, maxWait, pollEvery time.Duration) error {
	outs, err := stack.Outputs(ctx)
	if err != nil {
		return fmt.Errorf("read outputs for GitOps verification: %w", err)
	}
	lister, namespace, err := newGCPArgoListerFromOutputs(outs)
	if err != nil {
		return err
	}
	return waitForArgoApplicationsHealthy(ctx, lister, namespace, expected, maxWait, pollEvery)
}

func waitForAzureArgoApplicationsHealthy(ctx context.Context, stack auto.Stack, expected []argoApplicationExpectation, maxWait, pollEvery time.Duration) error {
	outs, err := stack.Outputs(ctx)
	if err != nil {
		return fmt.Errorf("read outputs for GitOps verification: %w", err)
	}
	lister, namespace, err := newAzureArgoListerFromOutputs(ctx, outs)
	if err != nil {
		return err
	}
	return waitForArgoApplicationsHealthy(ctx, lister, namespace, expected, maxWait, pollEvery)
}

func waitForArgoApplicationsHealthy(ctx context.Context, lister argoApplicationLister, namespace string, expected []argoApplicationExpectation, maxWait, pollEvery time.Duration) error {
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
			ready, why := argoApplicationsReady(apps, expected)
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

func argoApplicationsReady(apps []argoApplication, expected []argoApplicationExpectation) (bool, string) {
	if len(expected) == 0 {
		return false, "no expected Argo CD applications configured"
	}
	if len(apps) == 0 {
		return false, "no Argo CD applications reported yet"
	}
	var pending []string
	byName := make(map[string]argoApplication, len(apps))
	for _, app := range apps {
		if app.Metadata.Name != "" {
			byName[app.Metadata.Name] = app
		}
	}
	for _, want := range expected {
		app, ok := byName[want.name]
		if !ok {
			pending = append(pending, want.name+" missing")
			continue
		}
		if want.targetRevision == "" {
			pending = append(pending, want.name+" desired target revision is empty")
			continue
		}
		declared := argoApplicationTargetRevisions(app.Spec.Source, app.Spec.Sources)
		if !allTargetRevisionsEqual(declared, want.targetRevision) {
			pending = append(pending, fmt.Sprintf("%s declares target revisions %q, want %q", want.name, declared, want.targetRevision))
			continue
		}
		compared := app.Status.Sync.ComparedTo
		comparedTargets := argoApplicationTargetRevisions(compared.Source, compared.Sources)
		if len(comparedTargets) != len(declared) || !allTargetRevisionsEqual(comparedTargets, want.targetRevision) {
			pending = append(pending, fmt.Sprintf("%s last compared target revisions %q, want %q for %d sources", want.name, comparedTargets, want.targetRevision, len(declared)))
		}
	}
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

func argoApplicationTargetRevisions(source argoApplicationSource, sources []argoApplicationSource) []string {
	if len(sources) == 0 {
		return []string{strings.TrimSpace(source.TargetRevision)}
	}
	targets := make([]string, 0, len(sources))
	for _, item := range sources {
		targets = append(targets, strings.TrimSpace(item.TargetRevision))
	}
	return targets
}

func allTargetRevisionsEqual(got []string, want string) bool {
	if len(got) == 0 {
		return false
	}
	for _, revision := range got {
		if revision != want {
			return false
		}
	}
	return true
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

func (l *gcpArgoLister) clusterNodes(ctx context.Context) (total, ready int, ok bool) {
	token, err := l.tokenCmd(ctx, l.project)
	if err != nil {
		return 0, 0, false
	}
	return clusterNodesGet(ctx, l.client, l.baseURL, token)
}

// clusterReadyz probes the kube-apiserver's /readyz through the same
// authenticated client. A 200 "ok" is ready; a non-200 (500 lists the
// failing checks) is degraded; a transport error is "" reason, which
// the caller treats as unreachable.
func (l *gcpArgoLister) clusterReadyz(ctx context.Context) (bool, string) {
	token, err := l.tokenCmd(ctx, l.project)
	if err != nil {
		return false, ""
	}
	return clusterReadyzGet(ctx, l.client, l.baseURL, token)
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

type eksBearerToken struct {
	value     string
	expiresAt time.Time
}

type eksTokenProvider func(context.Context) (eksBearerToken, error)

// refreshingEKSBearerTransport owns the short-lived credential used by every
// AWS Kubernetes request. The cache is shared by post-up polling and the
// cell-health readyz/node/Application probes, so guard it for concurrent use.
type refreshingEKSBearerTransport struct {
	base          http.RoundTripper
	provider      eksTokenProvider
	now           func() time.Time
	refreshBefore time.Duration

	mu    sync.Mutex
	token eksBearerToken
}

// newAWSArgoListerFromOutputs builds a cluster prober for EKS. EKS
// doesn't hand back a kubeconfig with an embedded token the way GKE/AKS
// do, so we assemble one: DescribeCluster (SDK, cell profile) for the
// apiserver endpoint + CA, and `aws eks get-token` for a short-lived
// bearer — the same CLI-token pattern GCP (gcloud) and Azure (az)
// already use. The bearer transport refreshes before expiration and
// retries one 401 with a newly minted token, covering every lister probe.
func newAWSArgoListerFromOutputs(ctx context.Context, outs auto.OutputMap, region, profile string) (*tokenArgoLister, string, error) {
	clusterName := outputString(outs, "eksCluster")
	if clusterName == "" {
		return nil, "", fmt.Errorf("stack exports no eksCluster; cannot verify EKS health")
	}
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, "", fmt.Errorf("load AWS config: %w", err)
	}
	desc, err := eks.NewFromConfig(cfg).DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &clusterName})
	if err != nil {
		return nil, "", fmt.Errorf("describe EKS cluster: %w", err)
	}
	if desc.Cluster == nil || desc.Cluster.Endpoint == nil ||
		desc.Cluster.CertificateAuthority == nil || desc.Cluster.CertificateAuthority.Data == nil {
		return nil, "", fmt.Errorf("EKS DescribeCluster returned no endpoint/CA")
	}
	ca, err := base64.StdEncoding.DecodeString(*desc.Cluster.CertificateAuthority.Data)
	if err != nil {
		return nil, "", fmt.Errorf("decode EKS certificate authority: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, "", fmt.Errorf("EKS certificate authority did not contain PEM data")
	}
	provider := func(ctx context.Context) (eksBearerToken, error) {
		return awsEKSToken(ctx, clusterName, region, profile)
	}
	token, err := provider(ctx)
	if err != nil {
		return nil, "", err
	}
	namespace := outputString(outs, "argocdNamespace")
	if namespace == "" {
		namespace = "argocd"
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	return &tokenArgoLister{
		baseURL: strings.TrimRight(*desc.Cluster.Endpoint, "/"),
		client: &http.Client{Timeout: 30 * time.Second, Transport: &refreshingEKSBearerTransport{
			base:          transport,
			provider:      provider,
			now:           time.Now,
			refreshBefore: awsEKSTokenRefreshBefore,
			token:         token,
		}},
	}, namespace, nil
}

// awsEKSToken mints an EKS bearer token via the aws CLI, threading the
// cell's profile and region explicitly (the token-signing identity is
// the cluster's aws-auth mapping). get-token is local — it presigns an
// STS GetCallerIdentity, no cluster call — so it only needs valid
// credentials for the profile.
func awsEKSToken(ctx context.Context, cluster, region, profile string) (eksBearerToken, error) {
	args := []string{"eks", "get-token", "--cluster-name", cluster, "--output", "json"}
	if region != "" {
		args = append(args, "--region", region)
	}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	cmd := exec.CommandContext(ctx, "aws", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if d := strings.TrimSpace(stderr.String()); d != "" {
			return eksBearerToken{}, fmt.Errorf("aws eks get-token: %s", oneLine(d))
		}
		return eksBearerToken{}, fmt.Errorf("aws eks get-token: %w", err)
	}
	return parseEKSToken(out)
}

// parseEKSToken pulls the bearer token and its authoritative expiration out of
// `aws eks get-token --output json`.
func parseEKSToken(raw []byte) (eksBearerToken, error) {
	var resp struct {
		Status struct {
			Token               string `json:"token"`
			ExpirationTimestamp string `json:"expirationTimestamp"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return eksBearerToken{}, fmt.Errorf("parse EKS token: %w", err)
	}
	token := strings.TrimSpace(resp.Status.Token)
	if token == "" {
		return eksBearerToken{}, fmt.Errorf("aws eks get-token returned an empty token")
	}
	rawExpiration := strings.TrimSpace(resp.Status.ExpirationTimestamp)
	if rawExpiration == "" {
		return eksBearerToken{}, fmt.Errorf("aws eks get-token returned no expiration timestamp")
	}
	expiresAt, err := time.Parse(time.RFC3339, rawExpiration)
	if err != nil {
		return eksBearerToken{}, fmt.Errorf("parse EKS token expiration: %w", err)
	}
	return eksBearerToken{value: token, expiresAt: expiresAt}, nil
}

func (t *refreshingEKSBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.currentToken(req.Context(), false, "")
	if err != nil {
		return nil, fmt.Errorf("get EKS bearer token: %w", err)
	}
	resp, err := t.roundTrip(req, token.value)
	if err != nil {
		return resp, err
	}
	if resp == nil {
		return nil, fmt.Errorf("EKS transport returned no response")
	}
	if resp.StatusCode != http.StatusUnauthorized || req.Body != nil {
		return resp, nil
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	refreshed, err := t.currentToken(req.Context(), true, token.value)
	if err != nil {
		return nil, fmt.Errorf("refresh EKS bearer token after HTTP 401: %w", err)
	}
	return t.roundTrip(req, refreshed.value)
}

func (t *refreshingEKSBearerTransport) roundTrip(req *http.Request, token string) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

func (t *refreshingEKSBearerTransport) currentToken(ctx context.Context, force bool, rejected string) (eksBearerToken, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now
	if t.now != nil {
		now = t.now
	}
	current := t.token
	usable := current.value != "" && current.expiresAt.After(now().Add(t.refreshBefore))
	if usable && (!force || current.value != rejected) {
		return current, nil
	}
	if t.provider == nil {
		return eksBearerToken{}, fmt.Errorf("no EKS token provider configured")
	}
	fresh, err := t.provider(ctx)
	if err != nil {
		return eksBearerToken{}, err
	}
	if fresh.value == "" {
		return eksBearerToken{}, fmt.Errorf("EKS token provider returned an empty token")
	}
	if fresh.expiresAt.IsZero() {
		return eksBearerToken{}, fmt.Errorf("EKS token provider returned no expiration")
	}
	if !fresh.expiresAt.After(now()) {
		return eksBearerToken{}, fmt.Errorf("EKS token provider returned an expired token")
	}
	t.token = fresh
	return fresh, nil
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
	var clientCertificate tls.Certificate
	for _, user := range cfg.Users {
		token = strings.TrimSpace(user.User.Token)
		if token != "" {
			break
		}
		certData := strings.TrimSpace(user.User.ClientCertificateData)
		keyData := strings.TrimSpace(user.User.ClientKeyData)
		if certData == "" || keyData == "" {
			continue
		}
		certPEM, certErr := base64.StdEncoding.DecodeString(certData)
		if certErr != nil {
			return nil, fmt.Errorf("decode kubeconfig client certificate: %w", certErr)
		}
		keyPEM, keyErr := base64.StdEncoding.DecodeString(keyData)
		if keyErr != nil {
			return nil, fmt.Errorf("decode kubeconfig client key: %w", keyErr)
		}
		clientCertificate, err = tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("parse kubeconfig client certificate: %w", err)
		}
		break
	}
	if token == "" && len(clientCertificate.Certificate) == 0 {
		return nil, fmt.Errorf("kubeconfig contained no bearer token or client certificate")
	}
	tlsConfig := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	if len(clientCertificate.Certificate) > 0 {
		tlsConfig.Certificates = []tls.Certificate{clientCertificate}
	}
	return &tokenArgoLister{
		baseURL: strings.TrimRight(server, "/"),
		client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
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

func (l *tokenArgoLister) clusterReadyz(ctx context.Context) (bool, string) {
	return clusterReadyzGet(ctx, l.client, l.baseURL, l.token)
}

func (l *tokenArgoLister) clusterNodes(ctx context.Context) (total, ready int, ok bool) {
	return clusterNodesGet(ctx, l.client, l.baseURL, l.token)
}

// clusterNodesGet lists nodes and counts how many are Ready. ok is
// false on any transport/decoding error so the caller can leave the
// gauge blank rather than show a misleading 0/0.
func clusterNodesGet(ctx context.Context, client *http.Client, baseURL, token string) (total, ready int, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/v1/nodes", nil)
	if err != nil {
		return 0, 0, false
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, 0, false
	}
	var out struct {
		Items []struct {
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, 0, false
	}
	for _, n := range out.Items {
		total++
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready++
				break
			}
		}
	}
	return total, ready, true
}

// clusterReadyzGet performs the authenticated GET on /readyz shared by
// both listers. Ready is a 200 whose body is "ok"; a non-200 returns
// the body (kube-apiserver's 500 enumerates the failing checks); a
// transport error returns ("", false) so the caller can distinguish
// "unreachable" from "reachable but not ready".
func clusterReadyzGet(ctx context.Context, client *http.Client, baseURL, token string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/readyz?verbose", nil)
	if err != nil {
		return false, ""
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, ""
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK {
		return true, ""
	}
	body := strings.TrimSpace(string(raw))
	// The verbose body lists per-check lines; surface just the failing
	// ones so the Health line stays short.
	var failed []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "[-]") {
			failed = append(failed, strings.TrimPrefix(line, "[-]"))
		}
	}
	if len(failed) > 0 {
		return false, "failing checks: " + strings.Join(failed, ", ")
	}
	return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
}

func (l *tokenArgoLister) ListArgoApplications(ctx context.Context, namespace string) ([]argoApplication, error) {
	url := fmt.Sprintf("%s/apis/argoproj.io/v1alpha1/namespaces/%s/applications", l.baseURL, namespace)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if l.token != "" {
		req.Header.Set("Authorization", "Bearer "+l.token)
	}
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
			Token                 string `yaml:"token"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKeyData         string `yaml:"client-key-data"`
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
