package cell

import (
	"encoding/base64"
	"fmt"

	containerservice "github.com/pulumi/pulumi-azure-native-sdk/containerservice/v3"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apiextensions"
	helm "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/helm/v3"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// provisionAzureArgoCD installs the same portable Argo CD chart/root app used by
// AWS and GCP, but obtains the AKS kubeconfig from Azure Resource Manager. AKS
// returns a credential-bearing kubeconfig for this cluster shape, so the
// provider input is marked secret before Pulumi stores it in state.
func provisionAzureArgoCD(ctx *pulumi.Context, c azureCell, net *azureNetwork, aks *azureKubernetes, secrets *azureSecrets, eso *azureESO) error {
	credentials := containerservice.ListManagedClusterUserCredentialsOutput(ctx, containerservice.ListManagedClusterUserCredentialsOutputArgs{
		ResourceGroupName: net.resourceGroupName,
		ResourceName:      aks.name,
		Format:            pulumi.String("exec"),
	})

	kubeconfig := credentials.Kubeconfigs().Index(pulumi.Int(0)).Value().ApplyT(func(encoded string) (string, error) {
		if encoded == "" {
			return "", fmt.Errorf("AKS user kubeconfig is empty")
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("decode AKS user kubeconfig: %w", err)
		}
		return string(raw), nil
	}).(pulumi.StringOutput)
	kubeconfig = pulumi.ToSecret(kubeconfig).(pulumi.StringOutput)

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
	}, pulumi.Provider(k8s), pulumi.DeleteBeforeReplace(true), pulumi.DependsOn([]pulumi.Resource{aks.cluster}))
	if err != nil {
		return err
	}

	runtimeValues := pulumi.Sprintf(`gitops:
  repoURL: %q
  targetRevision: %q
  valuesPath: %q
platform:
  externalSecrets:
    azureVaultURL: %q
    serviceAccountAnnotations:
      azure.workload.identity/client-id: %q
      azure.workload.identity/tenant-id: %q
`, c.gitopsRepo, c.gitopsRevision, c.gitopsValuesPath, secrets.vaultURL, eso.clientID, eso.tenantID)

	rootDependsOn := append([]pulumi.Resource{release}, eso.dependencies...)

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
