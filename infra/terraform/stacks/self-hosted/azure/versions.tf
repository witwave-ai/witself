# Self-hosted Azure stack — provider and version constraints.
#
# SKELETON: AWS-first. Visible reference shape that composes the Azure module.
# Operators configure their own remote state backend; none is committed here.

terraform {
  required_version = ">= 1.6.0, < 2.0.0"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = ">= 3.95.0, < 4.0.0"
    }
  }

  # backend "azurerm" {
  #   resource_group_name  = "my-tfstate-rg"
  #   storage_account_name = "mytfstate"
  #   container_name       = "tfstate"
  #   key                  = "witself/self-hosted/azure.tfstate"
  # }
}

provider "azurerm" {
  features {}
}
