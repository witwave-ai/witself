package cell

import (
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	helm "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/helm/v3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Argo CD is installed from its upstream Helm chart so the install is identical
// on EKS, GKE, or a self-hosted cluster — the GitOps control plane is portable,
// not an AWS-only managed capability. The chart version is pinned; the bundled
// Argo CD app version comes with it.
const (
	argocdChart        = "argo-cd"
	argocdChartRepo    = "https://argoproj.github.io/argo-helm"
	argocdChartVersion = "10.0.1"
	argocdNamespace    = "argocd"
)

// provisionAWSArgoCD installs Argo CD into the cell's EKS cluster via Helm.
//
// The Kubernetes provider authenticates with an exec-based kubeconfig
// (`aws eks get-token`) rather than a static token: EKS Auto Mode provisions
// nodes on demand, so the first install can outlast a 15-minute token's TTL
// while Argo's pods wait for compute — exec refreshes automatically. The exec
// inherits the process AWS credentials (AWS_PROFILE / OIDC), the same chain
// every other cell resource uses, and authenticates as the principal running
// `up` — which holds cluster-admin via the cluster's bootstrap-creator grant.
// (A run by a different principal would lack access; configurable admin is a
// later slice.)
//
// This stands up the Argo CD control plane only. Pointing it at the .gitops/
// repo (the root ApplicationSet) is a later slice; for now the server stays
// ClusterIP — reach the UI with `kubectl port-forward`. SSO and ingress later.
func provisionAWSArgoCD(ctx *pulumi.Context, c awsCell, eks *awsEKS) error {
	kubeconfig := pulumi.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: cell
  cluster:
    server: %s
    certificate-authority-data: %s
contexts:
- name: cell
  context:
    cluster: cell
    user: cell
current-context: cell
users:
- name: cell
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args:
      - eks
      - get-token
      - --cluster-name
      - %s
      - --region
      - %s
      - --output
      - json
`, eks.endpoint, eks.certificateAuthority, eks.name, c.region)

	k8s, err := kubernetes.NewProvider(ctx, "cell-k8s", &kubernetes.ProviderArgs{
		Kubeconfig: kubeconfig,
	})
	if err != nil {
		return err
	}

	// Chart defaults are already non-HA / single-replica / ClusterIP, which is
	// what the minimal profile wants; the explicit ClusterIP documents intent.
	// HA-by-profile is a later slice. The release name "argocd" yields the
	// standard argocd-server/argocd-repo-server/... resources.
	_, err = helm.NewRelease(ctx, "argocd", &helm.ReleaseArgs{
		Chart:           pulumi.String(argocdChart),
		Version:         pulumi.String(argocdChartVersion),
		RepositoryOpts:  helm.RepositoryOptsArgs{Repo: pulumi.String(argocdChartRepo)},
		Namespace:       pulumi.String(argocdNamespace),
		CreateNamespace: pulumi.Bool(true),
		// Generous: Auto Mode may provision a node before Argo's pods go Ready.
		Timeout: pulumi.Int(900),
		Values: pulumi.Map{
			"server": pulumi.Map{
				"service": pulumi.Map{"type": pulumi.String("ClusterIP")},
			},
		},
	}, pulumi.Provider(k8s))
	if err != nil {
		return err
	}

	ctx.Export("argocdNamespace", pulumi.String(argocdNamespace))
	ctx.Export("argocdPortForward", pulumi.String("kubectl -n "+argocdNamespace+" port-forward svc/argocd-server 8080:443  # then https://localhost:8080 (user: admin)"))
	ctx.Export("argocdAdminSecret", pulumi.String("kubectl -n "+argocdNamespace+" get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d"))
	return nil
}
