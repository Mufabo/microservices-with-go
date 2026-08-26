# GKE Autopilot: Google manages nodes, scaling, and security posture; we pay
# per pod. Workload Identity is enabled by default on Autopilot.
resource "google_container_cluster" "autopilot" {
  name             = "snapvault"
  location         = var.region
  enable_autopilot = true

  # Required alongside enable_autopilot.
  ip_allocation_policy {}

  # Let Terraform delete the cluster on destroy in non-prod; flip on for prod.
  deletion_protection = false
}
