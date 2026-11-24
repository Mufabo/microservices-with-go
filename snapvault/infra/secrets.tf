# Secrets reach the Pods through GKE's Secret Manager add-on: a CSI driver that
# mounts Secret Manager secrets as files (deploy/k8s/overlays/prod/
# secretproviderclass.yaml). The services read them with config.MustString,
# which follows DATABASE_URL_FILE and MONGO_URI_FILE to the mounted files.

# MongoDB isn't a managed Google Cloud product. Run it on MongoDB Atlas (which
# has its own Terraform provider) or self-managed, and add its connection
# string out of band:
#
#   printf '%s' "$MONGO_URI" | gcloud secrets versions add snapvault-mongo-uri --data-file=-
resource "google_secret_manager_secret" "mongo_uri" {
  secret_id = "snapvault-mongo-uri"
  replication {
    auto {}
  }
}

data "google_project" "this" {}

locals {
  # The add-on reads a secret as the Pod's own Kubernetes service account
  # (snapvault/snapvault), through Workload Identity Federation for GKE, so
  # that principal needs access to each secret it mounts.
  runtime_ksa = "principal://iam.googleapis.com/projects/${data.google_project.this.number}/locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog/subject/ns/snapvault/sa/snapvault"

  mounted_secrets = {
    database_url = google_secret_manager_secret.database_url.id
    mongo_uri    = google_secret_manager_secret.mongo_uri.id
  }
}

resource "google_secret_manager_secret_iam_member" "runtime_ksa" {
  for_each  = local.mounted_secrets
  secret_id = each.value
  role      = "roles/secretmanager.secretAccessor"
  member    = local.runtime_ksa
}
