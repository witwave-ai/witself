package cell

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/rds"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// awsNetwork is the cell's dedicated network: its own VPC with public and private
// subnets across multiple AZs, a private DB subnet group, and a database security
// group. A cell owning its VPC is the network expression of cell isolation, and
// it removes any dependency on a region's default VPC — which does not exist in
// Control Tower-governed accounts.
type awsNetwork struct {
	vpcID         pulumi.StringOutput
	dbSubnetGroup pulumi.StringOutput // RDS subnet group name (private subnets)
	dbSecurityGrp pulumi.StringOutput // database security group id
}

// cidrPrefix returns the first two octets of a /16 CIDR (e.g. "10.20.0.0/16" ->
// "10.20"), used to carve /24 subnets. The cell CIDR is expected to be a /16.
func cidrPrefix(cidr string) string {
	o := strings.SplitN(cidr, ".", 3)
	if len(o) >= 2 {
		return o[0] + "." + o[1]
	}
	return "10.20"
}

// provisionAWSNetwork builds the cell VPC. Every resource is created through the
// provided provider (which stamps the cell defaultTags). The DB stage adds NO NAT
// gateway: the database lives in private subnets with no internet egress, so cell
// networking costs nothing beyond the (free) VPC, subnets, and internet gateway.
// NAT — or VPC endpoints — arrive with the EKS slice, when nodes need egress.
func provisionAWSNetwork(ctx *pulumi.Context, c awsCell, minimal bool, prov *aws.Provider) (*awsNetwork, error) {
	prefix := cidrPrefix(c.cidr)

	azs, err := aws.GetAvailabilityZones(ctx, &aws.GetAvailabilityZonesArgs{
		State: pulumi.StringRef("available"),
	}, pulumi.Provider(prov))
	if err != nil {
		return nil, err
	}
	azCount := 3
	if minimal {
		azCount = 2
	}
	if azCount > len(azs.Names) {
		azCount = len(azs.Names)
	}

	vpc, err := ec2.NewVpc(ctx, "cell", &ec2.VpcArgs{
		CidrBlock:          pulumi.String(c.cidr),
		EnableDnsHostnames: pulumi.Bool(true),
		EnableDnsSupport:   pulumi.Bool(true),
		Tags:               resourceTags(rname(c.name, "vpc"), "network"),
	}, pulumi.Provider(prov))
	if err != nil {
		return nil, err
	}

	igw, err := ec2.NewInternetGateway(ctx, "cell", &ec2.InternetGatewayArgs{
		VpcId: vpc.ID(),
		Tags:  resourceTags(rname(c.name, "igw"), "network"),
	}, pulumi.Provider(prov))
	if err != nil {
		return nil, err
	}

	publicRT, err := ec2.NewRouteTable(ctx, "cell-public", &ec2.RouteTableArgs{
		VpcId: vpc.ID(),
		Routes: ec2.RouteTableRouteArray{
			&ec2.RouteTableRouteArgs{
				CidrBlock: pulumi.String("0.0.0.0/0"),
				GatewayId: igw.ID(),
			},
		},
		Tags: resourceTags(rname(c.name, "public-rt"), "network"),
	}, pulumi.Provider(prov))
	if err != nil {
		return nil, err
	}

	// Private route table: local-only for now (no NAT until egress is needed).
	privateRT, err := ec2.NewRouteTable(ctx, "cell-private", &ec2.RouteTableArgs{
		VpcId: vpc.ID(),
		Tags:  resourceTags(rname(c.name, "private-rt"), "network"),
	}, pulumi.Provider(prov))
	if err != nil {
		return nil, err
	}

	var privateSubnets pulumi.StringArray
	for i := 0; i < azCount; i++ {
		az := azs.Names[i]

		pub, err := ec2.NewSubnet(ctx, fmt.Sprintf("cell-public-%d", i), &ec2.SubnetArgs{
			VpcId:               vpc.ID(),
			CidrBlock:           pulumi.String(fmt.Sprintf("%s.%d.0/24", prefix, i)),
			AvailabilityZone:    pulumi.String(az),
			MapPublicIpOnLaunch: pulumi.Bool(true),
			Tags: pulumi.StringMap{
				"Name":                   pulumi.String(rname(c.name, fmt.Sprintf("public-%d", i))),
				"witself:component":      pulumi.String("network"),
				"kubernetes.io/role/elb": pulumi.String("1"), // future EKS public load balancers
			},
		}, pulumi.Provider(prov))
		if err != nil {
			return nil, err
		}
		if _, err := ec2.NewRouteTableAssociation(ctx, fmt.Sprintf("cell-public-%d", i), &ec2.RouteTableAssociationArgs{
			SubnetId:     pub.ID(),
			RouteTableId: publicRT.ID(),
		}, pulumi.Provider(prov)); err != nil {
			return nil, err
		}

		priv, err := ec2.NewSubnet(ctx, fmt.Sprintf("cell-private-%d", i), &ec2.SubnetArgs{
			VpcId:            vpc.ID(),
			CidrBlock:        pulumi.String(fmt.Sprintf("%s.%d.0/24", prefix, i+10)),
			AvailabilityZone: pulumi.String(az),
			Tags: pulumi.StringMap{
				"Name":                            pulumi.String(rname(c.name, fmt.Sprintf("private-%d", i))),
				"witself:component":               pulumi.String("network"),
				"kubernetes.io/role/internal-elb": pulumi.String("1"), // future EKS internal load balancers
			},
		}, pulumi.Provider(prov))
		if err != nil {
			return nil, err
		}
		if _, err := ec2.NewRouteTableAssociation(ctx, fmt.Sprintf("cell-private-%d", i), &ec2.RouteTableAssociationArgs{
			SubnetId:     priv.ID(),
			RouteTableId: privateRT.ID(),
		}, pulumi.Provider(prov)); err != nil {
			return nil, err
		}

		privateSubnets = append(privateSubnets, priv.ID())
	}

	dbSG, err := ec2.NewSecurityGroup(ctx, "cell-db", &ec2.SecurityGroupArgs{
		Name:        pulumi.String(rname(c.name, "db")),
		VpcId:       vpc.ID(),
		Description: pulumi.String("witself cell database access"),
		Ingress: ec2.SecurityGroupIngressArray{
			&ec2.SecurityGroupIngressArgs{
				Description: pulumi.String("postgres from within the cell VPC"),
				Protocol:    pulumi.String("tcp"),
				FromPort:    pulumi.Int(5432),
				ToPort:      pulumi.Int(5432),
				CidrBlocks:  pulumi.StringArray{pulumi.String(c.cidr)},
			},
		},
		Egress: ec2.SecurityGroupEgressArray{
			&ec2.SecurityGroupEgressArgs{
				Protocol:   pulumi.String("-1"),
				FromPort:   pulumi.Int(0),
				ToPort:     pulumi.Int(0),
				CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
			},
		},
		Tags: resourceTags(rname(c.name, "db"), "database"),
	}, pulumi.Provider(prov))
	if err != nil {
		return nil, err
	}

	dbSubnetGroup, err := rds.NewSubnetGroup(ctx, "cell", &rds.SubnetGroupArgs{
		Name:        pulumi.String(rname(c.name, "db-subnets")),
		SubnetIds:   privateSubnets,
		Description: pulumi.String("witself cell private subnets"),
		Tags:        resourceTags(rname(c.name, "db-subnets"), "database"),
	}, pulumi.Provider(prov))
	if err != nil {
		return nil, err
	}

	return &awsNetwork{
		vpcID:         vpc.ID().ToStringOutput(),
		dbSubnetGroup: dbSubnetGroup.Name,
		dbSecurityGrp: dbSG.ID().ToStringOutput(),
	}, nil
}
