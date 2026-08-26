output "uploads_bucket" {
  description = "GCS bucket for image storage."
  value       = google_storage_bucket.uploads.name
}

output "artifact_registry" {
  description = "Docker image prefix; push images here."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.snapvault.repository_id}"
}

output "wif_provider" {
  description = "Workload Identity provider resource name for the Forgejo workflow."
  value       = google_iam_workload_identity_pool_provider.forgejo.name
}

output "deployer_service_account" {
  description = "Service account the CI workflow impersonates."
  value       = google_service_account.deployer.email
}

output "runtime_service_account" {
  description = "Service account the services run as."
  value       = google_service_account.runtime.email
}

output "gke_cluster" {
  value = google_container_cluster.autopilot.name
}
