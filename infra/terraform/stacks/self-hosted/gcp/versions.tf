# Self-hosted GCP stack — provider and version constraints.
#
# SKELETON: AWS-first. Visible reference shape that composes the GCP module.
# Operators configure their own remote state backend; none is committed here.

terraform {
  required_version = ">= 1.6.0, < 2.0.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 5.20.0, < 6.0.0"
    }
  }

  # backend "gcs" {
  #   bucket = "my-tfstate-bucket"
  #   prefix = "witself/self-hosted/gcp"
  # }
}

provider "google" {
  project = var.project_id
  region  = var.region
}
