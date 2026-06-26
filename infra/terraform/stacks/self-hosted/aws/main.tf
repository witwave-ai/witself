# Self-hosted AWS stack — composes the AWS module into a reference deployment.
#
# This is the shape an operator copies and adapts. The AWS module provisions the
# substrate; the operator then installs the witself Helm chart onto it using the
# outputs from this stack. See docs/self-hosting.md and docs/helm-chart.md.

module "witself" {
  source = "../../../modules/aws"

  name   = var.name
  region = var.region
  tags   = var.tags

  # Sealed plane (secrets + TOTP). KMS is provisioned only when this is true; an
  # open-plane-only deployment (memories + facts) needs no KMS.
  sealed_plane_enabled = var.sealed_plane_enabled
  existing_kms_key_arn = var.existing_kms_key_arn

  public_hostname = var.public_hostname
}
