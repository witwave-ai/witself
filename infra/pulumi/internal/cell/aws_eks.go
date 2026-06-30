package cell

import (
	"fmt"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/eks"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// awsEKS is the cell's Kubernetes layer.
type awsEKS struct {
	name     pulumi.StringOutput
	endpoint pulumi.StringOutput
	// certificateAuthority is the base64 CA bundle, used to build a kubeconfig
	// for the in-cluster add-ons (e.g. Argo CD) provisioned via Helm.
	certificateAuthority pulumi.StringOutput
}

// EKS trust policies + the managed policies each role needs. Auto Mode lets EKS
// manage nodes, core add-ons, scaling, and patching, so the cluster role carries
// the compute/storage/networking/load-balancing policies and the node role is
// minimal.
const eksClusterTrust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"eks.amazonaws.com"},"Action":["sts:AssumeRole","sts:TagSession"]}]}`
const eksNodeTrust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

var eksClusterPolicies = []string{
	"arn:aws:iam::aws:policy/AmazonEKSClusterPolicy",
	"arn:aws:iam::aws:policy/AmazonEKSComputePolicy",
	"arn:aws:iam::aws:policy/AmazonEKSBlockStoragePolicy",
	"arn:aws:iam::aws:policy/AmazonEKSLoadBalancingPolicy",
	"arn:aws:iam::aws:policy/AmazonEKSNetworkingPolicy",
}
var eksNodePolicies = []string{
	"arn:aws:iam::aws:policy/AmazonEKSWorkerNodeMinimalPolicy",
	"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPullOnly",
}

// provisionAWSEKS creates the cell's EKS Auto Mode cluster: AWS manages the nodes
// (Karpenter under the hood), the core add-ons, scaling, and node patching, so
// there is little to no operational overhead. The cluster sits across the cell's
// public + private subnets; with Auto Mode + Cloudflare Tunnel the workloads stay
// private. The creator (the principal running this) gets cluster-admin via the API
// auth mode's bootstrap.
func provisionAWSEKS(ctx *pulumi.Context, c awsCell, net *awsNetwork, prov *aws.Provider) (*awsEKS, error) {
	clusterRole, err := iam.NewRole(ctx, "cell-eks-cluster", &iam.RoleArgs{
		Name:             pulumi.String(rname(c.name, "eks-cluster")),
		AssumeRolePolicy: pulumi.String(eksClusterTrust),
		Tags:             resourceTags(rname(c.name, "eks-cluster"), "kubernetes"),
	}, pulumi.Provider(prov))
	if err != nil {
		return nil, err
	}

	nodeRole, err := iam.NewRole(ctx, "cell-eks-node", &iam.RoleArgs{
		Name:             pulumi.String(rname(c.name, "eks-node")),
		AssumeRolePolicy: pulumi.String(eksNodeTrust),
		Tags:             resourceTags(rname(c.name, "eks-node"), "kubernetes"),
	}, pulumi.Provider(prov))
	if err != nil {
		return nil, err
	}

	var deps []pulumi.Resource
	for i, p := range eksClusterPolicies {
		a, err := iam.NewRolePolicyAttachment(ctx, fmt.Sprintf("cell-eks-cluster-%d", i), &iam.RolePolicyAttachmentArgs{
			Role:      clusterRole.Name,
			PolicyArn: pulumi.String(p),
		}, pulumi.Provider(prov))
		if err != nil {
			return nil, err
		}
		deps = append(deps, a)
	}
	for i, p := range eksNodePolicies {
		a, err := iam.NewRolePolicyAttachment(ctx, fmt.Sprintf("cell-eks-node-%d", i), &iam.RolePolicyAttachmentArgs{
			Role:      nodeRole.Name,
			PolicyArn: pulumi.String(p),
		}, pulumi.Provider(prov))
		if err != nil {
			return nil, err
		}
		deps = append(deps, a)
	}

	// EKS wants subnets in >=2 AZs across public + private.
	var subnets pulumi.StringArray
	subnets = append(subnets, net.publicSubnets...)
	subnets = append(subnets, net.privateSubnets...)

	cluster, err := eks.NewCluster(ctx, "cell", &eks.ClusterArgs{
		Name:    pulumi.String(rname(c.name, "")), // witself-<cell> (bare; EKS name propagates widely)
		RoleArn: clusterRole.Arn,
		Version: pulumi.String(c.k8sVersion),
		AccessConfig: &eks.ClusterAccessConfigArgs{
			AuthenticationMode: pulumi.String("API"),
			// The principal that runs `up` (the operator, or the CI OIDC role) gets
			// cluster-admin automatically — no manual access entry needed. Note:
			// this is a create-time setting, so it only applies to a freshly
			// created cluster.
			BootstrapClusterCreatorAdminPermissions: pulumi.Bool(true),
		},
		VpcConfig: &eks.ClusterVpcConfigArgs{
			SubnetIds:             subnets,
			EndpointPrivateAccess: pulumi.Bool(true),
			EndpointPublicAccess:  pulumi.Bool(true), // convenient for dev kubectl; tighten later
		},
		// Auto Mode: EKS runs and patches the nodes + core add-ons.
		ComputeConfig: &eks.ClusterComputeConfigArgs{
			Enabled:     pulumi.Bool(true),
			NodePools:   pulumi.StringArray{pulumi.String("general-purpose"), pulumi.String("system")},
			NodeRoleArn: nodeRole.Arn,
		},
		KubernetesNetworkConfig: &eks.ClusterKubernetesNetworkConfigArgs{
			ElasticLoadBalancing: &eks.ClusterKubernetesNetworkConfigElasticLoadBalancingArgs{
				Enabled: pulumi.Bool(true),
			},
		},
		StorageConfig: &eks.ClusterStorageConfigArgs{
			BlockStorage: &eks.ClusterStorageConfigBlockStorageArgs{
				Enabled: pulumi.Bool(true),
			},
		},
		BootstrapSelfManagedAddons: pulumi.Bool(false),
		Tags:                       resourceTags(rname(c.name, ""), "kubernetes"),
	}, pulumi.Provider(prov), pulumi.DependsOn(deps))
	if err != nil {
		return nil, err
	}

	return &awsEKS{
		name:                 cluster.Name,
		endpoint:             cluster.Endpoint,
		certificateAuthority: cluster.CertificateAuthority.Data().Elem(),
	}, nil
}
