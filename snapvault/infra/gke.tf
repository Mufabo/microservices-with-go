# GKE Autopilot: Google manages nodes, scaling, and security posture; we pay
# per pod. Workload Identity is enabled by default on Autopilot.
resource "google_container_cluster" "autopilot" {
  name             = "snapvault"
  location         = var.region
  enable_autopilot = true

  # Required alongside enable_autopilot.
  ip_allocation_policy {}

  # The Secret Manager add-on: a managed CSI driver that mounts Secret Manager
  # secrets into Pods as files (see secrets.tf).
  secret_manager_config {
    enabled = true
  }

  # Let Terraform delete the cluster on destroy in non-prod; flip on for prod.
  deletion_protection = false
}
