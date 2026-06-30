package cell

import (
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/eks"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const metricsServerAddon = "metrics-server"

// provisionAWSEKSAddons installs AWS-managed EKS add-ons that are cell
// substrate. EKS Auto Mode already provides pod networking, DNS, block storage,
// load balancing, and the Pod Identity agent, so keep this list intentionally
// small and avoid re-installing redundant core add-ons.
func provisionAWSEKSAddons(ctx *pulumi.Context, c awsCell, cluster *awsEKS, prov *aws.Provider) error {
	version, err := eks.GetAddonVersion(ctx, &eks.GetAddonVersionArgs{
		AddonName:         metricsServerAddon,
		KubernetesVersion: c.k8sVersion,
	}, pulumi.Provider(prov))
	if err != nil {
		return err
	}

	addon, err := eks.NewAddon(ctx, "cell-metrics-server", &eks.AddonArgs{
		ClusterName:              cluster.name,
		AddonName:                pulumi.String(metricsServerAddon),
		AddonVersion:             pulumi.String(version.Version),
		ResolveConflictsOnCreate: pulumi.String("OVERWRITE"),
		ResolveConflictsOnUpdate: pulumi.String("PRESERVE"),
		Tags:                     resourceTags(rname(c.name, "metrics-server"), "observability"),
	}, pulumi.Provider(prov))
	if err != nil {
		return err
	}

	ctx.Export("metricsServerAddon", addon.AddonVersion)
	return nil
}
