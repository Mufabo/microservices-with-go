# CI identity. With GitOps (ArgoCD pulls from the config repo), the pipeline's
# only job in Google Cloud is to push images — ArgoCD does the deploying — so
# this account is intentionally narrow.
resource "google_service_account" "deployer" {
  account_id   = "snapvault-deployer"
  display_name = "Snapvault CI deployer"
}

# Keyless federation: trust OIDC tokens minted by the Forgejo instance.
resource "google_iam_workload_identity_pool" "forgejo" {
  workload_identity_pool_id = "forgejo-pool"
  display_name              = "Forgejo CI"
}

resource "google_iam_workload_identity_pool_provider" "forgejo" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.forgejo.workload_identity_pool_id
  workload_identity_pool_provider_id = "forgejo-provider"
  display_name                       = "Forgejo OIDC"

  attribute_mapping = {
    "google.subject"       = "assertion.sub"
    "attribute.repository" = "assertion.repository"
  }

  # Only tokens issued for our repository may use this provider.
  attribute_condition = "assertion.repository == \"${var.forgejo_repo}\""

  oidc {
    issuer_uri = var.forgejo_issuer
  }
}

# Allow the workflow for our repo to impersonate the deployer.
resource "google_service_account_iam_member" "deployer_wif" {
  service_account_id = google_service_account.deployer.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.forgejo.name}/attribute.repository/${var.forgejo_repo}"
}

# The deployer may push images to Artifact Registry.
resource "google_artifact_registry_repository_iam_member" "deployer_push" {
  location   = google_artifact_registry_repository.snapvault.location
  repository = google_artifact_registry_repository.snapvault.name
  role       = "roles/artifactregistry.writer"
  member     = "serviceAccount:${google_service_account.deployer.email}"
}
