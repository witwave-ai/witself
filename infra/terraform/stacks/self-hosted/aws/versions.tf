# Self-hosted AWS stack — provider and version constraints.
#
# Reference deployment operators can copy or adapt. Composes the AWS module into
# a runnable shape. State backend is intentionally left to the operator (see the
# commented backend block) — no state, credentials, or real tfvars are committed.

terraform {
  required_version = ">= 1.6.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.40.0, < 6.0.0"
    }
  }

  # Operators configure their own remote state backend out of band. Never commit
  # a backend with real bucket/credentials. Example:
  #
  # backend "s3" {
  #   bucket = "my-tfstate-bucket"
  #   key    = "witself/self-hosted/aws/terraform.tfstate"
  #   region = "us-east-1"
  # }
}

provider "aws" {
  region = var.region
}
