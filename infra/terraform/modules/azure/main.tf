# Azure module — substrate for Witself.
#
# SKELETON: AWS is the first implementation target; Azure follows. The resource
# bodies are commented placeholders marking the frozen substrate shape. See
# docs/terraform-infrastructure.md "Azure Target".

locals {
  tags = merge(
    {
      "part-of"    = "witself"
      "managed-by" = "terraform"
    },
    var.tags,
  )

  # Open plane (memories + facts) never needs KMS; the sealed plane roots its
  # envelope in an Azure Key Vault key. Provisioned only when enabled.
  provision_kms = var.sealed_plane_enabled
}

###############################################################################
# Networking — VNet, subnets, network security groups, private networking.
###############################################################################

# resource "azurerm_virtual_network" "this" {
#   name                = "${var.name}-vnet"
#   resource_group_name = var.resource_group_name
#   location            = var.location
#   address_space       = ["10.0.0.0/16"]
#   tags                = local.tags
# }

###############################################################################
# Kubernetes cluster (AKS) — new cluster or integration with an existing one.
# Azure Workload Identity enabled for witself-server.
###############################################################################

# resource "azurerm_kubernetes_cluster" "this" {
#   name                = "${var.name}-aks"
#   resource_group_name = var.resource_group_name
#   location            = var.location
#   dns_prefix          = var.name
#   tags                = local.tags
# }

###############################################################################
# PostgreSQL (Azure Database for PostgreSQL) with pgvector — open plane system
# of record. pgvector is a hard gate for the open plane; enable the "vector"
# extension and surface dimensionality as an output. See docs/storage.md.
###############################################################################

# resource "azurerm_postgresql_flexible_server" "postgres" {
#   name                = "${var.name}-pg"
#   resource_group_name = var.resource_group_name
#   location            = var.location
#   version             = "16"
#   tags                = local.tags
# }

###############################################################################
# Object/blob storage (Azure Blob Storage) — exports, attachments, bundles,
# backups.
###############################################################################

# resource "azurerm_storage_account" "blob" {
#   name                     = replace("${var.name}blob", "-", "")
#   resource_group_name      = var.resource_group_name
#   location                 = var.location
#   account_tier             = "Standard"
#   account_replication_type = "LRS"
#   tags                     = local.tags
# }

###############################################################################
# Workload identity — witself-server deployment identity for Postgres, the
# embedding provider, Key Vault (sealed plane), and Blob Storage.
###############################################################################

# resource "azurerm_user_assigned_identity" "witself_server" {
#   name                = "${var.name}-server"
#   resource_group_name = var.resource_group_name
#   location            = var.location
#   tags                = local.tags
# }

###############################################################################
# Sealed plane — Azure Key Vault key (provisioned only when sealed_plane_enabled
# is true). Root of the envelope (CMK -> per-realm KEK -> per-secret/field DEK).
# Grant the witself-server workload identity wrap/unwrap (and encrypt/decrypt)
# key permissions — envelope operations only, never key administration.
###############################################################################

# resource "azurerm_key_vault" "sealed_plane" {
#   count               = local.provision_kms ? 1 : 0
#   name                = "${var.name}-kv"
#   resource_group_name = var.resource_group_name
#   location            = var.location
#   tenant_id           = data.azurerm_client_config.current.tenant_id
#   sku_name            = "standard"
#   tags                = local.tags
# }
#
# resource "azurerm_key_vault_key" "sealed_plane_cmk" {
#   count        = local.provision_kms ? 1 : 0
#   name         = "${var.name}-cmk"
#   key_vault_id = azurerm_key_vault.sealed_plane[0].id
#   key_type     = "RSA"
#   key_size     = 4096
#   key_opts     = ["wrapKey", "unwrapKey", "encrypt", "decrypt"]
# }
