# Witself Cloud AWS stack — provider and version constraints.
#
# Public-shape example of the managed Witself Cloud deployment on AWS. It shows
# the infrastructure shape and security posture reviewers should see WITHOUT
# exposing live credentials, real state backends, production account IDs, or
# environment-specific variables — those live outside the public repo.
#
# Managed Witself Cloud uses the same public AWS module as self-hosted, with
# stricter defaults (sealed plane on, public hostname, additional observability
# and abuse controls layered in private overlays). See
# docs/terraform-infrastructure.md "Self-Hosted Vs Witself Cloud".

terraform {
  required_version = ">= 1.6.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.40.0, < 6.0.0"
    }
  }

  # The real state backend and credentials live outside the public repo. Never
  # commit a backend with a real bucket, key, or account. Example only:
  #
  # backend "s3" {
  #   bucket = "REDACTED-tfstate"
  #   key    = "witself-cloud/aws/terraform.tfstate"
  #   region = "us-east-1"
  # }
}

provider "aws" {
  region = var.region
}
