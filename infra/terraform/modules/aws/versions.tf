# AWS module — provider and version constraints.
#
# This module is the first implementation target. The resource bodies in main.tf
# are commented placeholders for the scaffold; provider plugins are pinned now so
# `terraform init -backend=false` + `terraform validate` work without surprises
# once the real resources land.

terraform {
  required_version = ">= 1.6.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.40.0, < 6.0.0"
    }
    # Used to apply the EKS-issued kubeconfig (e.g. aws-auth) and to run the
    # pgvector CREATE EXTENSION bootstrap from inside the cluster network.
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.27.0, < 3.0.0"
    }
  }
}
