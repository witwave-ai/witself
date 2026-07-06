package cell

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/sql"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func gcpPostgresVersion(version string) string {
	v := strings.TrimSpace(strings.ToUpper(version))
	if v == "" {
		v = "18"
	}
	if strings.HasPrefix(v, "POSTGRES_") {
		return v
	}
	return "POSTGRES_" + v
}

func gcpDBTier(profile string) string {
	if profile == "prod" {
		return "db-custom-2-8192"
	}
	return "db-f1-micro"
}

func gcpDBSecretName(c gcpCell) string {
	return rname(c.name, "db")
}

func gcpDBAvailabilityType(minimal bool) string {
	if minimal {
		return "ZONAL"
	}
	return "REGIONAL"
}

func gcpDBBackupConfiguration(minimal bool) *sql.DatabaseInstanceSettingsBackupConfigurationArgs {
	if minimal {
		return &sql.DatabaseInstanceSettingsBackupConfigurationArgs{
			Enabled: pulumi.Bool(false),
		}
	}
	return &sql.DatabaseInstanceSettingsBackupConfigurationArgs{
		Enabled:                     pulumi.Bool(true),
		PointInTimeRecoveryEnabled:  pulumi.Bool(true),
		StartTime:                   pulumi.String("07:00"),
		TransactionLogRetentionDays: pulumi.Int(7),
		BackupRetentionSettings: &sql.DatabaseInstanceSettingsBackupConfigurationBackupRetentionSettingsArgs{
			RetainedBackups: pulumi.Int(7),
			RetentionUnit:   pulumi.String("COUNT"),
		},
	}
}

func provisionGCPCloudSQL(ctx *pulumi.Context, c gcpCell, net *gcpNetwork, prov *gcp.Provider, sqlAPI, secretManagerAPI pulumi.Resource) (*gcpDatabase, error) {
	minimal := c.profile != "prod"
	version := gcpPostgresVersion(c.dbVersion)
	dbName := "witself"
	dbUser := "witself"

	suffix, err := random.NewRandomId(ctx, "witself-db-suffix", &random.RandomIdArgs{
		ByteLength: pulumi.Int(3),
	})
	if err != nil {
		return nil, err
	}

	pw, err := random.NewRandomPassword(ctx, "witself-db", &random.RandomPasswordArgs{
		Length:  pulumi.Int(24),
		Special: pulumi.Bool(false),
	})
	if err != nil {
		return nil, err
	}

	instance, err := sql.NewDatabaseInstance(ctx, "witself", &sql.DatabaseInstanceArgs{
		Name:               pulumi.Sprintf("%s-%s", rname(c.name, "db"), suffix.Hex),
		Region:             pulumi.String(c.region),
		DatabaseVersion:    pulumi.String(version),
		DeletionPolicy:     pulumi.String("DELETE"),
		DeletionProtection: pulumi.Bool(false),
		Settings: &sql.DatabaseInstanceSettingsArgs{
			Tier:                      pulumi.String(gcpDBTier(c.profile)),
			Edition:                   pulumi.String("ENTERPRISE"),
			AvailabilityType:          pulumi.String(gcpDBAvailabilityType(minimal)),
			DiskType:                  pulumi.String("PD_SSD"),
			DiskSize:                  pulumi.Int(10),
			DiskAutoresize:            pulumi.Bool(true),
			DiskAutoresizeLimit:       pulumi.Int(20),
			DeletionProtectionEnabled: pulumi.Bool(false),
			RetainBackupsOnDelete:     pulumi.Bool(false),
			UserLabels:                gcpDefaultLabels(c),
			BackupConfiguration:       gcpDBBackupConfiguration(minimal),
			IpConfiguration: &sql.DatabaseInstanceSettingsIpConfigurationArgs{
				Ipv4Enabled:                             pulumi.Bool(false),
				PrivateNetwork:                          net.networkSelfLink,
				AllocatedIpRange:                        net.privateRangeName,
				EnablePrivatePathForGoogleCloudServices: pulumi.Bool(true),
				SslMode:                                 pulumi.String("ENCRYPTED_ONLY"),
			},
		},
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{sqlAPI, net.privateConnection}))
	if err != nil {
		return nil, err
	}

	database, err := sql.NewDatabase(ctx, "witself", &sql.DatabaseArgs{
		Name:      pulumi.String(dbName),
		Instance:  instance.Name,
		Project:   pulumi.String(c.project),
		Charset:   pulumi.String("UTF8"),
		Collation: pulumi.String("en_US.UTF8"),
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{instance}))
	if err != nil {
		return nil, err
	}

	user, err := sql.NewUser(ctx, "witself", &sql.UserArgs{
		Name:     pulumi.String(dbUser),
		Instance: instance.Name,
		Project:  pulumi.String(c.project),
		Password: pw.Result,
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{instance}))
	if err != nil {
		return nil, err
	}

	dsn := pulumi.All(instance.PrivateIpAddress, pw.Result).ApplyT(func(a []interface{}) string {
		host, password := a[0].(string), a[1].(string)
		return fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=require", dbUser, password, host, dbName)
	}).(pulumi.StringOutput)

	payload := pulumi.All(instance.PrivateIpAddress, pw.Result, dsn).ApplyT(func(a []interface{}) (string, error) {
		host, password, conn := a[0].(string), a[1].(string), a[2].(string)
		b, err := json.Marshal(map[string]interface{}{
			"host":     host,
			"port":     5432,
			"username": dbUser,
			"password": password,
			"dbname":   dbName,
			"dsn":      conn,
		})
		return string(b), err
	}).(pulumi.StringOutput)

	secret, err := secretmanager.NewSecret(ctx, "witself-db", &secretmanager.SecretArgs{
		SecretId:           pulumi.String(gcpDBSecretName(c)),
		DeletionPolicy:     pulumi.String("DELETE"),
		DeletionProtection: pulumi.Bool(false),
		Labels:             gcpDefaultLabels(c),
		Replication: &secretmanager.SecretReplicationArgs{
			Auto: &secretmanager.SecretReplicationAutoArgs{},
		},
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{secretManagerAPI}))
	if err != nil {
		return nil, err
	}

	if _, err := secretmanager.NewSecretVersion(ctx, "witself-db", &secretmanager.SecretVersionArgs{
		Secret:     secret.ID(),
		SecretData: payload,
	}, pulumi.Provider(prov), pulumi.DependsOn([]pulumi.Resource{database, user, secret})); err != nil {
		return nil, err
	}

	return &gcpDatabase{
		instanceName:   instance.Name,
		connectionName: instance.ConnectionName,
		privateIP:      instance.PrivateIpAddress,
		databaseName:   database.Name,
		username:       pulumi.String(dbUser).ToStringOutput(),
		password:       pw.Result,
		dsn:            dsn,
		secretName:     pulumi.String(gcpDBSecretName(c)).ToStringOutput(),
		secretID:       secret.ID(),
		version:        version,
	}, nil
}
