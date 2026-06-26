# Witself Cloud AWS stack — managed deployment shape.
#
# Reuses the same public AWS module as self-hosted, with stricter managed
# defaults. The sealed plane (secrets + TOTP) is ON by default for managed Cloud,
# so the KMS envelope root is always provisioned (or brought via BYOK). Additional
# observability, abuse controls, and deployment pipelines are layered in private
# overlays outside this public repo. See docs/terraform-infrastructure.md.

module "witself" {
  source = "../../../modules/aws"

  name   = var.name
  region = var.region
  tags   = var.tags

  # Managed Cloud always runs both planes: the sealed plane is enabled, so KMS is
  # required. Operators of the managed plane keep the CMK lifecycle in a private
  # account and pass it as BYOK; a fresh CMK is provisioned only if none is given.
  sealed_plane_enabled = true
  existing_kms_key_arn = var.existing_kms_key_arn

  public_hostname = var.public_hostname
}
