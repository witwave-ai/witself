# GCP module — substrate for Witself.
#
# SKELETON: AWS is the first implementation target; GCP follows. The resource
# bodies are commented placeholders marking the frozen substrate shape. See
# docs/terraform-infrastructure.md "GCP Target".

locals {
  labels = merge(
    {
      "part-of"    = "witself"
      "managed-by" = "terraform"
    },
    var.labels,
  )

  # Open plane (memories + facts) never needs KMS; the sealed plane roots its
  # envelope in a Cloud KMS key. KMS is provisioned only when enabled.
  provision_kms = var.sealed_plane_enabled
}

###############################################################################
# Networking — VPC network, subnets, firewall prerequisites.
###############################################################################

# resource "google_compute_network" "this" {
#   name                    = "${var.name}-net"
#   auto_create_subnetworks = false
# }

###############################################################################
# Kubernetes cluster (GKE) — new cluster or integration with an existing one.
###############################################################################

# resource "google_container_cluster" "this" {
#   name     = "${var.name}-gke"
#   location = var.region
#   # Workload Identity enabled for witself-server.
# }

###############################################################################
# PostgreSQL (Cloud SQL) with pgvector — open plane system of record.
# pgvector is a hard gate for the open plane. Enable the "vector" extension and
# surface dimensionality as an output. See docs/storage.md.
###############################################################################

# resource "google_sql_database_instance" "postgres" {
#   name             = "${var.name}-pg"
#   database_version = "POSTGRES_16"
#   region           = var.region
# }

###############################################################################
# Object/blob storage (Cloud Storage) — exports, attachments, bundles, backups.
###############################################################################

# resource "google_storage_bucket" "blob" {
#   name     = "${var.name}-blob"
#   location = var.region
#   labels   = local.labels
# }

###############################################################################
# Workload Identity — witself-server deployment identity for Postgres, the
# embedding provider, KMS (sealed plane), and Cloud Storage.
###############################################################################

# resource "google_service_account" "witself_server" {
#   account_id   = "${var.name}-server"
#   display_name = "witself-server"
# }

###############################################################################
# Sealed plane — Cloud KMS (provisioned only when sealed_plane_enabled is true).
#
# Root of the envelope (CMK -> per-realm KEK -> per-secret/field DEK). Grant the
# witself-server workload identity roles/cloudkms.cryptoKeyEncrypterDecrypter on
# the key — envelope operations only, never key administration.
###############################################################################

# resource "google_kms_key_ring" "sealed_plane" {
#   count    = local.provision_kms ? 1 : 0
#   name     = "${var.name}-sealed-plane"
#   location = var.region
# }
#
# resource "google_kms_crypto_key" "sealed_plane" {
#   count    = local.provision_kms ? 1 : 0
#   name     = "${var.name}-cmk"
#   key_ring = google_kms_key_ring.sealed_plane[0].id
#   # rotation_period set for the CMK
# }
