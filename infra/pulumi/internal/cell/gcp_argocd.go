package cell

import (
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apiextensions"
	helm "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/helm/v3"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// provisionGCPArgoCD installs the same portable Argo CD chart/root app used on
// AWS, but authenticates to GKE with an exec kubeconfig backed by Application
// Default Credentials. The GCS backend and gcpkms secrets provider already
// require ADC, so using the same credential path avoids depending on stale
// interactive gcloud login state during Argo/Helm operations.
func provisionGCPArgoCD(ctx *pulumi.Context, c gcpCell, gke *gcpKubernetes, dns *gcpDNS, externalDNSServiceAccountEmail pulumi.StringOutput, rootDependencies ...pulumi.Resource) error {
	tokenExec := `token="$(gcloud auth application-default print-access-token)"
printf '{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential","status":{"token":"%s"}}\n' "$token"
`
	kubeconfig := pulumi.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: cell
  cluster:
    server: https://%s
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
      command: sh
      args:
      - -c
      - %q
      env:
      - name: CLOUDSDK_CORE_PROJECT
        value: %q
`, gke.endpoint, gke.certificateAuthority, tokenExec, c.project)

	k8s, err := kubernetes.NewProvider(ctx, "cell-k8s", &kubernetes.ProviderArgs{
		Kubeconfig: kubeconfig,
	})
	if err != nil {
		return err
	}

	release, err := helm.NewRelease(ctx, "argocd", &helm.ReleaseArgs{
		Name:            pulumi.String(argocdNamespace),
		Chart:           pulumi.String(argocdChart),
		Version:         pulumi.String(argocdChartVersion),
		RepositoryOpts:  helm.RepositoryOptsArgs{Repo: pulumi.String(argocdChartRepo)},
		Namespace:       pulumi.String(argocdNamespace),
		CreateNamespace: pulumi.Bool(true),
		Timeout:         pulumi.Int(900),
		Values: pulumi.Map{
			"server": pulumi.Map{
				"service": pulumi.Map{"type": pulumi.String("ClusterIP")},
			},
		},
	}, pulumi.Provider(k8s), pulumi.DeleteBeforeReplace(true))
	if err != nil {
		return err
	}

	cellDomain := pulumi.String("").ToStringOutput()
	apiHost := pulumi.String("").ToStringOutput()
	ingressStaticIPName := pulumi.String("").ToStringOutput()
	if dns != nil {
		cellDomain = pulumi.String(dns.zoneName).ToStringOutput()
		apiHost = pulumi.String(dns.apiHost).ToStringOutput()
		ingressStaticIPName = dns.apiAddressName
	}

	runtimeValues := pulumi.Sprintf(`gitops:
  repoURL: %q
  targetRevision: %q
  valuesPath: %q
cell:
  domain: %q
  apiHost: %q
apps:
  witselfServer:
    gcpIngress:
      staticIPName: %q
platform:
  externalDNS:
    serviceAccountAnnotations:
      iam.gke.io/gcp-service-account: %q
`, c.gitopsRepo, c.gitopsRevision, c.gitopsValuesPath, cellDomain, apiHost, ingressStaticIPName, externalDNSServiceAccountEmail)

	rootDependsOn := append([]pulumi.Resource{release}, rootDependencies...)

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
	}, pulumi.Provider(k8s), pulumi.DependsOn(rootDependsOn))
	if err != nil {
		return err
	}

	ctx.Export("argocdNamespace", pulumi.String(argocdNamespace))
	ctx.Export("argocdPortForward", pulumi.String("kubectl -n "+argocdNamespace+" port-forward svc/argocd-server 8080:443  # then https://localhost:8080 (user: admin)"))
	ctx.Export("argocdAdminSecret", pulumi.String("kubectl -n "+argocdNamespace+" get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d"))
	ctx.Export("gitops", pulumi.String(c.gitopsRepo+" @ "+c.gitopsRevision+" ("+c.gitopsPath+" + "+c.gitopsValuesPath+")"))
	return nil
}
