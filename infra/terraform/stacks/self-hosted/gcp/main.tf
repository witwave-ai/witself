# Self-hosted GCP stack — composes the GCP module.
#
# SKELETON: AWS-first. The GCP module is a visible placeholder; this stack shows
# the composition shape. See docs/terraform-infrastructure.md "GCP Target".

module "witself" {
  source = "../../../modules/gcp"

  name                 = var.name
  project_id           = var.project_id
  region               = var.region
  sealed_plane_enabled = var.sealed_plane_enabled
}
