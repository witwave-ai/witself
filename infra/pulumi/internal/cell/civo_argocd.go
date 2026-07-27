package cell

import (
	"github.com/pulumi/pulumi-civo/sdk/v2/go/civo"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apiextensions"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	helm "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/helm/v3"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const civoWorkloadNamespace = "witself"

func provisionCivoArgoCD(ctx *pulumi.Context, c civoCell, cluster *civo.KubernetesCluster, cellDomain, apiHost pulumi.StringInput, rootDependencies ...pulumi.Resource) error {
	k8s, err := kubernetes.NewProvider(ctx, "cell-k8s", &kubernetes.ProviderArgs{
		Kubeconfig: cluster.Kubeconfig,
	}, pulumi.DependsOn([]pulumi.Resource{cluster}))
	if err != nil {
		return err
	}

	workloadNamespace, err := corev1.NewNamespace(ctx, "witself-namespace", &corev1.NamespaceArgs{
		Metadata: &metav1.ObjectMetaArgs{Name: pulumi.String(civoWorkloadNamespace)},
	}, pulumi.Provider(k8s))
	if err != nil {
		return err
	}

	dbPassword, err := random.NewRandomPassword(ctx, "civo-postgres-password", &random.RandomPasswordArgs{
		Length:  pulumi.Int(32),
		Special: pulumi.Bool(false),
	})
	if err != nil {
		return err
	}
	credentials, err := newCellCredentials(ctx)
	if err != nil {
		return err
	}

	var bootstrapToken pulumi.StringOutput
	if c.bootstrapTokenSet {
		bootstrapToken = c.bootstrapToken
	} else {
		bootstrapBody, err := random.NewRandomString(ctx, "witself-bootstrap-token", &random.RandomStringArgs{
			Length: pulumi.Int(43), Special: pulumi.Bool(false),
			Upper: pulumi.Bool(true), Lower: pulumi.Bool(true), Numeric: pulumi.Bool(true),
		})
		if err != nil {
			return err
		}
		bootstrapToken = bootstrapBody.Result.ApplyT(func(body string) string {
			return "witself_boot_" + body
		}).(pulumi.StringOutput)
	}

	secretOpts := []pulumi.ResourceOption{
		pulumi.Provider(k8s),
		pulumi.DependsOn([]pulumi.Resource{workloadNamespace}),
		pulumi.AdditionalSecretOutputs([]string{"data", "stringData"}),
	}
	postgresAuth, err := corev1.NewSecret(ctx, "civo-postgres-auth", &corev1.SecretArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("witself-postgresql-auth"),
			Namespace: pulumi.String(civoWorkloadNamespace),
		},
		StringData: pulumi.StringMap{
			"password":          dbPassword.Result,
			"postgres-password": dbPassword.Result,
		},
		Type: pulumi.String("Opaque"),
	}, secretOpts...)
	if err != nil {
		return err
	}
	dbDSN := pulumi.Sprintf("postgres://witself:%s@witself-postgresql.%s.svc.cluster.local:5432/witself?sslmode=disable", dbPassword.Result, civoWorkloadNamespace)
	dbSecret, err := corev1.NewSecret(ctx, "witself-db", &corev1.SecretArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("witself-db"),
			Namespace: pulumi.String(civoWorkloadNamespace),
		},
		StringData: pulumi.StringMap{"dsn": dbDSN},
		Type:       pulumi.String("Opaque"),
	}, secretOpts...)
	if err != nil {
		return err
	}
	bootstrapSecret, err := corev1.NewSecret(ctx, "witself-bootstrap", &corev1.SecretArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("witself-bootstrap"),
			Namespace: pulumi.String(civoWorkloadNamespace),
		},
		StringData: pulumi.StringMap{"token": bootstrapToken, "ttl": pulumi.String("24h")},
		Type:       pulumi.String("Opaque"),
	}, secretOpts...)
	if err != nil {
		return err
	}
	provisionSecret, err := corev1.NewSecret(ctx, "witself-provision", &corev1.SecretArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("witself-provision"),
			Namespace: pulumi.String(civoWorkloadNamespace),
		},
		StringData: pulumi.StringMap{"token": credentials.provisionToken},
		Type:       pulumi.String("Opaque"),
	}, secretOpts...)
	if err != nil {
		return err
	}
	// Civo stores its machine credentials directly in Kubernetes rather than
	// synchronizing a provider JSON secret through ESO. Keep the existing
	// provisioning Secret byte-for-byte compatible and add backup authority in
	// its own Secret so enabling snapshots cannot delete/recreate the live
	// provisioning credential.
	backupSecret, err := corev1.NewSecret(ctx, "witself-backup", &corev1.SecretArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("witself-backup"),
			Namespace: pulumi.String(civoWorkloadNamespace),
		},
		StringData: pulumi.StringMap{
			"backup_token": credentials.backupToken,
		},
		Type: pulumi.String("Opaque"),
	}, secretOpts...)
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
		Values:          argocdReleaseValues(),
	}, pulumi.Provider(k8s), pulumi.DeleteBeforeReplace(true))
	if err != nil {
		return err
	}

	runtimeValues := pulumi.Sprintf(`gitops:
  repoURL: %q
  targetRevision: %q
  valuesPath: %q
cell:
  cloud: civo
  domain: %q
  apiHost: %q
platform:
  externalDNS:
    enabled: false
`, c.gitopsRepo, c.gitopsRevision, c.gitopsValuesPath, cellDomain, apiHost)

	rootDependsOn := append([]pulumi.Resource{
		release,
		postgresAuth,
		dbSecret,
		bootstrapSecret,
		provisionSecret,
		backupSecret,
	}, rootDependencies...)
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
						"repoURL": c.gitopsRepo, "targetRevision": c.gitopsRevision, "path": c.gitopsPath,
						"helm": map[string]interface{}{
							"valueFiles": []interface{}{"$values/" + c.gitopsValuesPath},
							"values":     runtimeValues,
						},
					},
					map[string]interface{}{"repoURL": c.gitopsRepo, "targetRevision": c.gitopsRevision, "ref": "values"},
				},
				"destination": map[string]interface{}{"server": "https://kubernetes.default.svc", "namespace": argocdNamespace},
				"syncPolicy":  map[string]interface{}{"automated": map[string]interface{}{"prune": true, "selfHeal": true}},
			},
		},
	}, pulumi.Provider(k8s), pulumi.DependsOn(rootDependsOn))
	if err != nil {
		return err
	}

	ctx.Export("argocdNamespace", pulumi.String(argocdNamespace))
	ctx.Export("gitops", pulumi.String(c.gitopsRepo+" @ "+c.gitopsRevision+" ("+c.gitopsPath+" + "+c.gitopsValuesPath+")"))
	ctx.Export("dbInstance", pulumi.String("witself-postgresql"))
	ctx.Export("provisionToken", pulumi.ToSecret(credentials.provisionToken))
	ctx.Export("backupToken", pulumi.ToSecret(credentials.backupToken))
	return nil
}
