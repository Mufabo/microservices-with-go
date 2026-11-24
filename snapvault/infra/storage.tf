# Object storage for image originals and thumbnails.
resource "google_storage_bucket" "uploads" {
  name                        = "snapvault-uploads-prod"
  location                    = var.region
  uniform_bucket_level_access = true
  force_destroy               = false

  # Originals and thumbnails are the user's photos, and feed rows point at
  # them, so they are never deleted by age. After a year they move to the
  # cheaper Nearline class, which still serves reads, at a per-read fee.
  lifecycle_rule {
    condition { age = 365 }
    action {
      type          = "SetStorageClass"
      storage_class = "NEARLINE"
    }
  }
}

# Container registry for the service images.
resource "google_artifact_registry_repository" "snapvault" {
  location      = var.region
  repository_id = "snapvault"
  format        = "DOCKER"
  description   = "Snapvault service images"
}
