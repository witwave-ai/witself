package cell

import "testing"

func TestAzureAKSNodeProfileForMinimal(t *testing.T) {
	p := azureAKSNodeProfileFor("minimal")
	if p.vmSize != "Standard_D2s_v4" {
		t.Fatalf("minimal vm size = %q, want Standard_D2s_v4", p.vmSize)
	}
	if p.minCount != 1 {
		t.Fatalf("minimal min count = %d, want 1", p.minCount)
	}
	if p.maxCount != 20 {
		t.Fatalf("minimal max count = %d, want 20", p.maxCount)
	}
}

func TestAzureAKSNodeProfileForProd(t *testing.T) {
	p := azureAKSNodeProfileFor("prod")
	if p.vmSize != "Standard_D2s_v4" {
		t.Fatalf("prod vm size = %q, want Standard_D2s_v4", p.vmSize)
	}
	if p.minCount != 2 {
		t.Fatalf("prod min count = %d, want 2", p.minCount)
	}
	if p.maxCount != 20 {
		t.Fatalf("prod max count = %d, want 20", p.maxCount)
	}
}

func TestAzureAKSNodeProfileForUnknownProfileUsesMinimalShape(t *testing.T) {
	p := azureAKSNodeProfileFor("dev")
	if p.minCount != 1 || p.maxCount != 20 {
		t.Fatalf("unknown profile counts = %d..%d, want 1..20", p.minCount, p.maxCount)
	}
}
