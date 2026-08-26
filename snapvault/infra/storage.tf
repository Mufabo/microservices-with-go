# Object storage for image originals and thumbnails.
resource "google_storage_bucket" "uploads" {
  name                        = "snapvault-uploads-prod"
  location                    = var.region
  uniform_bucket_level_access = true
  force_destroy               = false

  lifecycle_rule {
    condition { age = 365 }
    action { type = "Delete" }
  }
}

# Container registry for the service images.
resource "google_artifact_registry_repository" "snapvault" {
  location      = var.region
  repository_id = "snapvault"
  format        = "DOCKER"
  description   = "Snapvault service images"
}
