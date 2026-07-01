package cell

import (
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/acm"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/eks"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/iam"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/route53"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	externalDNSNamespace      = "external-dns"
	externalDNSServiceAccount = "external-dns"
)

type awsDNS struct {
	zoneName              string
	apiHost               string
	zoneID                pulumi.StringOutput
	ingressCertificateARN pulumi.StringOutput
}

func normalizeZoneName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

func provisionAWSDNS(ctx *pulumi.Context, c awsCell, prov *aws.Provider) (*awsDNS, error) {
	parentDomain := normalizeZoneName(c.domain)
	if parentDomain == "" {
		return nil, nil
	}
	zoneName := c.name + "." + parentDomain
	apiHost := "api." + zoneName

	zone, err := route53.NewZone(ctx, "cell-dns", &route53.ZoneArgs{
		Name:         pulumi.StringPtr(zoneName),
		Comment:      pulumi.StringPtr("Witself cell public DNS zone for " + c.name),
		ForceDestroy: pulumi.BoolPtr(true),
		Tags:         resourceTags(rname(c.name, "dns"), "dns"),
	}, pulumi.Provider(prov))
	if err != nil {
		return nil, err
	}

	cert, err := acm.NewCertificate(ctx, "cell-api", &acm.CertificateArgs{
		DomainName:       pulumi.StringPtr(apiHost),
		ValidationMethod: pulumi.StringPtr("DNS"),
		Tags:             resourceTags(rname(c.name, "api"), "tls"),
	}, pulumi.Provider(prov), pulumi.DeleteBeforeReplace(true))
	if err != nil {
		return nil, err
	}

	validation := cert.DomainValidationOptions.Index(pulumi.Int(0))
	validationRecord, err := route53.NewRecord(ctx, "cell-api-cert-validation", &route53.RecordArgs{
		ZoneId:         zone.ZoneId,
		Name:           validation.ResourceRecordName().Elem(),
		Type:           validation.ResourceRecordType().Elem(),
		Ttl:            pulumi.IntPtr(60),
		AllowOverwrite: pulumi.BoolPtr(true),
		Records: pulumi.StringArray{
			validation.ResourceRecordValue().Elem(),
		},
	}, pulumi.Provider(prov))
	if err != nil {
		return nil, err
	}
	ingressCertificateARN := pulumi.All(cert.Status, cert.Arn).ApplyT(func(args []interface{}) string {
		if args[0].(string) != "ISSUED" {
			return ""
		}
		return args[1].(string)
	}).(pulumi.StringOutput)

	ctx.Export("cellDomain", pulumi.String(zoneName))
	ctx.Export("apiHost", pulumi.String(apiHost))
	ctx.Export("dnsZoneName", pulumi.String(zoneName))
	ctx.Export("dnsZoneID", zone.ZoneId)
	ctx.Export("dnsZoneNameServers", zone.NameServers)
	ctx.Export("dnsDelegationRecordName", pulumi.String(zoneName))
	ctx.Export("dnsDelegationRecordType", pulumi.String("NS"))
	ctx.Export("tlsCertificateARN", cert.Arn)
	ctx.Export("tlsCertificateStatus", cert.Status)
	ctx.Export("tlsValidationRecord", validationRecord.Fqdn)

	return &awsDNS{
		zoneName:              zoneName,
		apiHost:               apiHost,
		zoneID:                zone.ZoneId,
		ingressCertificateARN: ingressCertificateARN,
	}, nil
}

func provisionAWSExternalDNSPodIdentity(ctx *pulumi.Context, c awsCell, cluster *awsEKS, dns *awsDNS, prov *aws.Provider) error {
	if dns == nil {
		return nil
	}

	role, err := iam.NewRole(ctx, "cell-external-dns", &iam.RoleArgs{
		Name:             pulumi.String(rname(c.name, "external-dns")),
		AssumeRolePolicy: pulumi.String(esoPodIdentityTrust),
		Tags:             resourceTags(rname(c.name, "external-dns"), "dns"),
	}, pulumi.Provider(prov))
	if err != nil {
		return err
	}

	policyDoc := pulumi.Sprintf(`{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "route53:ChangeResourceRecordSets",
      "Resource": "arn:aws:route53:::hostedzone/%s"
    },
    {
      "Effect": "Allow",
      "Action": [
        "route53:GetChange",
        "route53:ListHostedZones",
        "route53:ListHostedZonesByName",
        "route53:ListResourceRecordSets",
        "route53:ListTagsForResources"
      ],
      "Resource": "*"
    }
  ]
}`, dns.zoneID)

	if _, err := iam.NewRolePolicy(ctx, "cell-external-dns", &iam.RolePolicyArgs{
		Name:   pulumi.String(rname(c.name, "external-dns")),
		Role:   role.ID(),
		Policy: policyDoc,
	}, pulumi.Provider(prov)); err != nil {
		return err
	}

	if _, err := eks.NewPodIdentityAssociation(ctx, "cell-external-dns", &eks.PodIdentityAssociationArgs{
		ClusterName:    cluster.name,
		Namespace:      pulumi.String(externalDNSNamespace),
		ServiceAccount: pulumi.String(externalDNSServiceAccount),
		RoleArn:        role.Arn,
		Tags:           resourceTags(rname(c.name, "external-dns"), "dns"),
	}, pulumi.Provider(prov)); err != nil {
		return err
	}

	ctx.Export("externalDNSRole", role.Arn)
	ctx.Export("externalDNSZone", pulumi.String(dns.zoneName))
	return nil
}
