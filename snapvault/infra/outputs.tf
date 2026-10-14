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

output "cloudsql_connection_name" {
  description = "Instance connection name for the Cloud SQL Auth Proxy (prod overlay)."
  value       = google_sql_database_instance.postgres.connection_name
}

output "gke_cluster" {
  value = google_container_cluster.autopilot.name
}

output "auth_issuer" {
  description = "AUTH_ISSUER for the gateway: the issuer of Identity Platform ID tokens."
  value       = "https://securetoken.google.com/${var.project_id}"
}

output "auth_audience" {
  description = "AUTH_AUDIENCE for the gateway: Identity Platform ID tokens name the project ID as audience."
  value       = var.project_id
}
