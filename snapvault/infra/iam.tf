# Runtime identity used by the services in the cluster.
resource "google_service_account" "runtime" {
  account_id   = "snapvault"
  display_name = "Snapvault runtime"
}

# Read/write the uploads bucket.
resource "google_storage_bucket_iam_member" "runtime_bucket" {
  bucket = google_storage_bucket.uploads.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.runtime.email}"
}

# Export traces and metrics.
resource "google_project_iam_member" "runtime_trace" {
  project = var.project_id
  role    = "roles/cloudtrace.agent"
  member  = "serviceAccount:${google_service_account.runtime.email}"
}

resource "google_project_iam_member" "runtime_metrics" {
  project = var.project_id
  role    = "roles/monitoring.metricWriter"
  member  = "serviceAccount:${google_service_account.runtime.email}"
}

# Workload Identity: let the Kubernetes SA snapvault/snapvault act as this GSA,
# so pods authenticate to Google APIs with no key file.
resource "google_service_account_iam_member" "runtime_wi" {
  service_account_id = google_service_account.runtime.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[snapvault/snapvault]"
}
