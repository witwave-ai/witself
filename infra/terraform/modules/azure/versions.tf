# Azure module — provider and version constraints.
#
# SKELETON: Azure is a planned follow-up target. AWS is implemented first. This
# module is a visible placeholder so the layout and outputs surface exist; the
# resource bodies in main.tf are commented. See docs/terraform-infrastructure.md.

terraform {
  required_version = ">= 1.6.0, < 2.0.0"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = ">= 3.95.0, < 4.0.0"
    }
  }
}
