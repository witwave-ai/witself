# AWS module — substrate for Witself.
#
# SCAFFOLD: the resource bodies below are commented placeholders. They mark the
# frozen shape of the substrate (cluster, Postgres+pgvector, blob storage,
# sealed-plane KMS, workload identity, networking) without provisioning anything.
# Concrete resources land with the implementation pass; the structure here is the
# contract reviewers can read today. See docs/terraform-infrastructure.md.

locals {
  tags = merge(
    {
      "app.kubernetes.io/part-of" = "witself"
      "witself.io/managed-by"     = "terraform"
      "witself.io/component"      = "substrate"
    },
    var.tags,
  )

  # The open plane (memories + facts) is ordinary data-at-rest and never needs
  # KMS. The sealed plane (secrets + TOTP) roots its envelope in a CMK. KMS is
  # provisioned only when the sealed plane is enabled.
  provision_kms = var.sealed_plane_enabled
}

###############################################################################
# Networking
#
# VPC, private subnets across AZs, security groups, and the network policy
# prerequisites. Sized to leave room for a post-v0 messaging transport (broker,
# streaming backend, or cross-pod fan-out) without re-architecting the stack.
###############################################################################

# module "network" {
#   source = "terraform-aws-modules/vpc/aws"
#   ...
#   name = "${var.name}-vpc"
#   cidr = var.vpc_cidr
#   # private subnets across >= 2 AZs for EKS + RDS
#   tags = local.tags
# }

###############################################################################
# Kubernetes cluster (EKS)
#
# Provision a new EKS cluster, or integrate with an existing one when
# create_cluster is false. The witself-server pod needs no Kubernetes API access
# for ordinary identity/policy/group/messaging operations; cluster RBAC stays
# minimal (the chart enforces authorization at the application layer).
###############################################################################

# module "eks" {
#   source  = "terraform-aws-modules/eks/aws"
#   ...
#   cluster_name    = var.create_cluster ? "${var.name}-eks" : var.existing_cluster_name
#   cluster_version = var.kubernetes_version
#   enable_irsa     = true
#   tags            = local.tags
# }

###############################################################################
# PostgreSQL (RDS/Aurora) with pgvector — open plane system of record
#
# pgvector is a hard gate for the open plane. Engine version must ship pgvector;
# add "vector" to shared_preload_libraries via a managed parameter group; run
# CREATE EXTENSION IF NOT EXISTS vector as a bootstrap step (init Job, bootstrap
# SQL, or witself-server migrate on first start). Surface vector extension state
# and dimensionality as outputs. See docs/storage.md.
###############################################################################

# resource "aws_db_parameter_group" "postgres" {
#   name_prefix = "${var.name}-pg"
#   family      = "postgres16"
#   # pgvector requires "vector" in shared_preload_libraries on some engines.
#   parameter {
#     name         = "shared_preload_libraries"
#     value        = "vector"
#     apply_method = "pending-reboot"
#   }
#   tags = local.tags
# }

# resource "aws_rds_cluster" "postgres" {
#   count              = var.postgres_engine == "aurora-postgresql" ? 1 : 0
#   cluster_identifier = "${var.name}-pg"
#   engine             = var.postgres_engine
#   engine_version     = var.postgres_engine_version
#   database_name      = var.database_name
#   # Master credentials are managed out of band (Secrets Manager / generated),
#   # never hard-coded here and never rendered into Helm values. See README.
#   tags = local.tags
# }

# The pgvector bootstrap (CREATE EXTENSION IF NOT EXISTS vector) is owned by
# witself-server migrate (Goose, advisory lock). Terraform ensures the engine
# and role permit the extension; it does not own the extension lifecycle.

###############################################################################
# Object/blob storage (S3) — exports, attachments, diagnostic bundles, backups
###############################################################################

# resource "aws_s3_bucket" "blob" {
#   count  = var.create_blob_storage ? 1 : 0
#   bucket = "${var.name}-blob"
#   tags   = local.tags
# }
#
# resource "aws_s3_bucket_public_access_block" "blob" {
#   count                   = var.create_blob_storage ? 1 : 0
#   bucket                  = aws_s3_bucket.blob[0].id
#   block_public_acls       = true
#   block_public_policy     = true
#   ignore_public_acls      = true
#   restrict_public_buckets = true
# }

###############################################################################
# Workload identity (IRSA) — witself-server deployment identity
#
# The IRSA role lets the witself-server ServiceAccount reach Postgres, the
# embedding provider, KMS (sealed plane), and S3 without static credentials.
# Surfaced to Helm as a ServiceAccount annotation.
###############################################################################

# data "aws_iam_policy_document" "irsa_trust" {
#   statement {
#     actions = ["sts:AssumeRoleWithWebIdentity"]
#     principals {
#       type        = "Federated"
#       identifiers = [module.eks.oidc_provider_arn]
#     }
#     condition {
#       test     = "StringEquals"
#       variable = "${module.eks.oidc_provider}:sub"
#       values   = ["system:serviceaccount:${var.kubernetes_namespace}:${var.service_account_name}"]
#     }
#   }
# }
#
# resource "aws_iam_role" "irsa" {
#   name               = "${var.name}-witself-server"
#   assume_role_policy = data.aws_iam_policy_document.irsa_trust.json
#   tags               = local.tags
# }

###############################################################################
# Sealed plane — KMS (provisioned only when sealed_plane_enabled is true)
#
# Root of the envelope hierarchy: CMK -> per-realm KEK -> per-secret/field DEK.
# Terraform never sees a KEK or DEK. The submodule grants the IRSA role envelope
# operations only (Encrypt, Decrypt, GenerateDataKey,
# GenerateDataKeyWithoutPlaintext, DescribeKey) — never key administration or
# deletion. Operators may bring an existing CMK by ARN (BYOK).
###############################################################################

module "kms" {
  source = "./modules/kms"
  count  = local.provision_kms ? 1 : 0

  name                 = var.name
  existing_kms_key_arn = var.existing_kms_key_arn
  key_rotation_enabled = var.kms_key_rotation_enabled
  # Wiring note (implementation pass): pass the IRSA role ARN so the submodule
  # scopes the envelope-operations policy to the deployment identity.
  # deployment_role_arn = aws_iam_role.irsa.arn
  deployment_role_arn = ""
  tags                = local.tags
}

###############################################################################
# Optional DNS / TLS (Route 53 + ACM) — enabled when public_hostname is set
###############################################################################

# resource "aws_acm_certificate" "public" {
#   count             = var.public_hostname != "" ? 1 : 0
#   domain_name       = var.public_hostname
#   validation_method = "DNS"
#   tags              = local.tags
# }
