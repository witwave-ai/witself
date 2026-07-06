package cell

import "testing"

func TestGCPDBProfileForMinimal(t *testing.T) {
	p := gcpDBProfileFor("minimal")

	if p.tier != "db-f1-micro" {
		t.Fatalf("tier = %q, want db-f1-micro", p.tier)
	}
	if p.availabilityType != "ZONAL" {
		t.Fatalf("availabilityType = %q, want ZONAL", p.availabilityType)
	}
	if p.diskSizeGB != 10 {
		t.Fatalf("diskSizeGB = %d, want 10", p.diskSizeGB)
	}
	if p.diskAutoresizeLimitGB != 20 {
		t.Fatalf("diskAutoresizeLimitGB = %d, want 20", p.diskAutoresizeLimitGB)
	}
	if p.retainBackupsOnDelete {
		t.Fatal("minimal profile should not retain backups on delete")
	}
	if p.finalBackupRetentionDays != 0 {
		t.Fatalf("finalBackupRetentionDays = %d, want 0", p.finalBackupRetentionDays)
	}
}

func TestGCPDBProfileForProd(t *testing.T) {
	p := gcpDBProfileFor("prod")

	if p.tier != "db-custom-2-8192" {
		t.Fatalf("tier = %q, want db-custom-2-8192", p.tier)
	}
	if p.availabilityType != "REGIONAL" {
		t.Fatalf("availabilityType = %q, want REGIONAL", p.availabilityType)
	}
	if p.diskSizeGB != 100 {
		t.Fatalf("diskSizeGB = %d, want 100", p.diskSizeGB)
	}
	if p.diskAutoresizeLimitGB != 500 {
		t.Fatalf("diskAutoresizeLimitGB = %d, want 500", p.diskAutoresizeLimitGB)
	}
	if !p.retainBackupsOnDelete {
		t.Fatal("prod profile should retain backups on delete")
	}
	if p.finalBackupRetentionDays != 30 {
		t.Fatalf("finalBackupRetentionDays = %d, want 30", p.finalBackupRetentionDays)
	}
}

func TestGCPDBProfileForUnknownDefaultsToMinimal(t *testing.T) {
	p := gcpDBProfileFor("staging")

	if p.tier != "db-f1-micro" {
		t.Fatalf("tier = %q, want db-f1-micro", p.tier)
	}
	if p.availabilityType != "ZONAL" {
		t.Fatalf("availabilityType = %q, want ZONAL", p.availabilityType)
	}
}
