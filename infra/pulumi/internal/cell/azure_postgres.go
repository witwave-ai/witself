package cell

import (
	"strings"

	dbforpostgresql "github.com/pulumi/pulumi-azure-native-sdk/dbforpostgresql/v3"
	privatedns "github.com/pulumi/pulumi-azure-native-sdk/privatedns/v3"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const azurePostgresPrivateZone = "privatelink.postgres.database.azure.com"

type azureDBProfile struct {
	skuName             string
	tier                string
	storageGB           int
	backupRetentionDays int
	geoRedundantBackup  string
}

func azurePostgresVersion(version string) string {
	v := strings.TrimSpace(version)
	if v == "" {
		return "18"
	}
	return strings.TrimPrefix(strings.ToUpper(v), "POSTGRES_")
}

func azureDBProfileFor(profile string) azureDBProfile {
	if profile == "prod" {
		return azureDBProfile{
			skuName:             "Standard_D2ds_v5",
			tier:                "GeneralPurpose",
			storageGB:           128,
			backupRetentionDays: 7,
			geoRedundantBackup:  "Disabled",
		}
	}

	return azureDBProfile{
		skuName:             "Standard_B1ms",
		tier:                "Burstable",
		storageGB:           32,
		backupRetentionDays: 7,
		geoRedundantBackup:  "Disabled",
	}
}

func provisionAzurePostgres(ctx *pulumi.Context, c azureCell, net *azureNetwork) (*azureDatabase, error) {
	dbProfile := azureDBProfileFor(c.profile)
	version := azurePostgresVersion(c.dbVersion)
	dbName := "witself"
	dbUser := "witself"

	suffix, err := random.NewRandomId(ctx, "witself-db-suffix", &random.RandomIdArgs{
		ByteLength: pulumi.Int(3),
	})
	if err != nil {
		return nil, err
	}

	pw, err := random.NewRandomPassword(ctx, "witself-db", &random.RandomPasswordArgs{
		Length:     pulumi.Int(24),
		Special:    pulumi.Bool(false),
		MinLower:   pulumi.Int(1),
		MinNumeric: pulumi.Int(1),
		MinUpper:   pulumi.Int(1),
	})
	if err != nil {
		return nil, err
	}

	zone, err := privatedns.NewPrivateZone(ctx, "cell-postgres", &privatedns.PrivateZoneArgs{
		ResourceGroupName: net.resourceGroupName,
		PrivateZoneName:   pulumi.String(azurePostgresPrivateZone),
		Location:          pulumi.String("global"),
		Tags:              azureResourceTags(c, azurePostgresPrivateZone, "database"),
	})
	if err != nil {
		return nil, err
	}

	zoneLinkName := rname(c.name, "postgres")
	zoneLink, err := privatedns.NewVirtualNetworkLink(ctx, "cell-postgres", &privatedns.VirtualNetworkLinkArgs{
		ResourceGroupName:      net.resourceGroupName,
		PrivateZoneName:        zone.Name,
		VirtualNetworkLinkName: pulumi.String(zoneLinkName),
		Location:               pulumi.String("global"),
		RegistrationEnabled:    pulumi.Bool(false),
		VirtualNetwork: privatedns.SubResourceArgs{
			Id: net.vnetID,
		},
		Tags: azureResourceTags(c, zoneLinkName, "database"),
	}, pulumi.DependsOn([]pulumi.Resource{zone}))
	if err != nil {
		return nil, err
	}

	serverName := pulumi.Sprintf("%s-%s", rname(c.name, "db"), suffix.Hex)
	server, err := dbforpostgresql.NewServer(ctx, "witself", &dbforpostgresql.ServerArgs{
		ResourceGroupName:          net.resourceGroupName,
		ServerName:                 serverName,
		Location:                   pulumi.String(c.region),
		Version:                    pulumi.String(version),
		AdministratorLogin:         pulumi.String(dbUser),
		AdministratorLoginPassword: pw.Result,
		AuthConfig: dbforpostgresql.AuthConfigArgs{
			ActiveDirectoryAuth: pulumi.String("Disabled"),
			PasswordAuth:        pulumi.String("Enabled"),
		},
		Backup: dbforpostgresql.BackupTypeArgs{
			BackupRetentionDays: pulumi.Int(dbProfile.backupRetentionDays),
			GeoRedundantBackup:  pulumi.String(dbProfile.geoRedundantBackup),
		},
		CreateMode: pulumi.String("Default"),
		HighAvailability: dbforpostgresql.HighAvailabilityArgs{
			Mode: pulumi.String("Disabled"),
		},
		Network: dbforpostgresql.NetworkArgs{
			DelegatedSubnetResourceId:   net.dbSubnetID,
			PrivateDnsZoneArmResourceId: zone.ID(),
			PublicNetworkAccess:         pulumi.String("Disabled"),
		},
		Sku: dbforpostgresql.SkuArgs{
			Name: pulumi.String(dbProfile.skuName),
			Tier: pulumi.String(dbProfile.tier),
		},
		Storage: dbforpostgresql.StorageArgs{
			AutoGrow:      pulumi.String("Enabled"),
			StorageSizeGB: pulumi.Int(dbProfile.storageGB),
			Type:          pulumi.String("Premium_LRS"),
		},
		Tags: azureResourceTags(c, rname(c.name, "db"), "database"),
	}, pulumi.DependsOn([]pulumi.Resource{zoneLink}), pulumi.DeleteBeforeReplace(true))
	if err != nil {
		return nil, err
	}

	database, err := dbforpostgresql.NewDatabase(ctx, "witself", &dbforpostgresql.DatabaseArgs{
		ResourceGroupName: net.resourceGroupName,
		ServerName:        server.Name,
		DatabaseName:      pulumi.String(dbName),
	}, pulumi.DependsOn([]pulumi.Resource{server}))
	if err != nil {
		return nil, err
	}

	dsn := pulumi.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=require", dbUser, pw.Result, server.FullyQualifiedDomainName, dbName)

	return &azureDatabase{
		serverName:     server.Name,
		fqdn:           server.FullyQualifiedDomainName,
		databaseName:   database.Name,
		username:       pulumi.String(dbUser).ToStringOutput(),
		password:       pw.Result,
		dsn:            dsn,
		privateDNSZone: zone.Name,
		privateDNSLink: zoneLink.Name,
		version:        version,
		database:       database,
	}, nil
}
