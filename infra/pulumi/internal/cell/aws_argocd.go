package cell

import (
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apiextensions"
	helm "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/helm/v3"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
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

	// Default GitOps source Argo reconciles, overridable via the -gitops-* flags
	// so a self-hoster can point Argo at their own fork / config repo. The repo is
	// public, so no credentials are needed to read it (private-repo creds: issue #7).
	DefaultGitopsRepo     = "https://github.com/witwave-ai/witself"
	DefaultGitopsPath     = ".gitops/charts/bootstrap"
	DefaultGitopsRevision = "main"
)

const argocdApplicationHealthLua = `hs = {}
hs.status = "Progressing"
hs.message = ""
if obj.status ~= nil then
  if obj.status.health ~= nil then
    hs.status = obj.status.health.status
    if obj.status.health.message ~= nil then
      hs.message = obj.status.health.message
    end
  end
end
return hs
`

func argocdReleaseValues() pulumi.Map {
	return pulumi.Map{
		"configs": pulumi.Map{
			"cm": pulumi.Map{
				"resource.customizations.health.argoproj.io_Application": pulumi.String(argocdApplicationHealthLua),
			},
		},
		"server": pulumi.Map{
			"service": pulumi.Map{"type": pulumi.String("ClusterIP")},
		},
	}
}

// DefaultGitopsValuesPath is where a cell's bootstrap values live in
// the GitOps repo when no explicit path is given.
func DefaultGitopsValuesPath(cellName string) string {
	return ".gitops/cells/" + cellName + "/values.yaml"
}

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
// It also creates the root Argo Application ("bootstrap") pointing at the cell's
// GitOps bootstrap path, so Argo reconciles that cell's GitOps tree with no
// credentials. The server stays ClusterIP — reach the UI with `kubectl
// port-forward`. SSO and ingress are later slices.
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
	release, err := helm.NewRelease(ctx, "argocd", &helm.ReleaseArgs{
		// Pin the release name. Without it Pulumi auto-names the release with a
		// random suffix (argocd-<hash>), so the resources become
		// argocd-<hash>-server/... — non-deterministic names that break the
		// exported helper commands and the future GitOps/ingress wiring, which
		// reference the stable argocd-server / argocd-repo-server / ... names.
		Name:            pulumi.String(argocdNamespace),
		Chart:           pulumi.String(argocdChart),
		Version:         pulumi.String(argocdChartVersion),
		RepositoryOpts:  helm.RepositoryOptsArgs{Repo: pulumi.String(argocdChartRepo)},
		Namespace:       pulumi.String(argocdNamespace),
		CreateNamespace: pulumi.Bool(true),
		// Generous: Auto Mode may provision a node before Argo's pods go Ready.
		Timeout: pulumi.Int(900),
		Values:  argocdReleaseValues(),
		// Delete-before-replace: the argo-cd chart has fixed-name resources
		// (argocd-cm, argocd-rbac-cm, argocd-secret, the CRDs) that are NOT
		// release-prefixed, so a create-before-delete replacement would collide
		// with the outgoing release on those names. Tear down first, then create.
	}, pulumi.Provider(k8s), pulumi.DeleteBeforeReplace(true))
	if err != nil {
		return err
	}

	// Root "app of apps": one Argo Application renders the reusable cell
	// bootstrap chart with this cell's values file. Git owns the child
	// Applications, versions, and stable DNS names; Pulumi only ensures Argo
	// exists, points at the right Git source, and passes generated cloud outputs.
	// DependsOn the release so the Application CRD that the chart installs exists
	// before we create this CR.
	runtimeValues := pulumi.Sprintf(`gitops:
  repoURL: %q
  targetRevision: %q
  valuesPath: %q
cell:
  tlsCertificateARN: %q
`, c.gitopsRepo, c.gitopsRevision, c.gitopsValuesPath, c.tlsCertificateARN)

	_, err = apiextensions.NewCustomResource(ctx, "argocd-root", &apiextensions.CustomResourceArgs{
		ApiVersion: pulumi.String("argoproj.io/v1alpha1"),
		Kind:       pulumi.String("Application"),
		Metadata: &metav1.ObjectMetaArgs{
			Name:       pulumi.String("bootstrap"),
			Namespace:  pulumi.String(argocdNamespace),
			Finalizers: pulumi.StringArray{pulumi.String("resources-finalizer.argocd.argoproj.io")},
		},
		OtherFields: kubernetes.UntypedArgs{
			"spec": map[string]interface{}{
				"project": "default",
				"sources": []interface{}{
					map[string]interface{}{
						"repoURL":        c.gitopsRepo,
						"targetRevision": c.gitopsRevision,
						"path":           c.gitopsPath,
						"helm": map[string]interface{}{
							"valueFiles": []interface{}{"$values/" + c.gitopsValuesPath},
							"values":     runtimeValues,
						},
					},
					map[string]interface{}{
						"repoURL":        c.gitopsRepo,
						"targetRevision": c.gitopsRevision,
						"ref":            "values",
					},
				},
				"destination": map[string]interface{}{
					"server":    "https://kubernetes.default.svc",
					"namespace": argocdNamespace,
				},
				"syncPolicy": map[string]interface{}{
					"automated": map[string]interface{}{"prune": true, "selfHeal": true},
				},
			},
		},
	}, pulumi.Provider(k8s), pulumi.DependsOn([]pulumi.Resource{release}), pulumi.Timeouts(&pulumi.CustomTimeouts{
		Delete: "30m",
	}))
	if err != nil {
		return err
	}

	ctx.Export("argocdNamespace", pulumi.String(argocdNamespace))
	ctx.Export("argocdPortForward", pulumi.String("kubectl -n "+argocdNamespace+" port-forward svc/argocd-server 8080:443  # then https://localhost:8080 (user: admin)"))
	ctx.Export("argocdAdminSecret", pulumi.String("kubectl -n "+argocdNamespace+" get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d"))
	ctx.Export("gitops", pulumi.String(c.gitopsRepo+" @ "+c.gitopsRevision+" ("+c.gitopsPath+" + "+c.gitopsValuesPath+")"))
	return nil
}
