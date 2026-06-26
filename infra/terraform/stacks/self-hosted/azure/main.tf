# Self-hosted Azure stack — composes the Azure module.
#
# SKELETON: AWS-first. The Azure module is a visible placeholder; this stack
# shows the composition shape. See docs/terraform-infrastructure.md "Azure Target".

module "witself" {
  source = "../../../modules/azure"

  name                 = var.name
  resource_group_name  = var.resource_group_name
  location             = var.location
  sealed_plane_enabled = var.sealed_plane_enabled
}
