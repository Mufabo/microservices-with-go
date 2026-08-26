terraform {
  required_version = ">= 1.9"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.13"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  # Remote state in GCS. Provide the bucket at init time:
  #   terraform init -backend-config="bucket=YOUR_TF_STATE_BUCKET"
  backend "gcs" {
    prefix = "snapvault/prod"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

data "google_client_config" "default" {}

# The helm provider talks to the GKE cluster created below.
provider "helm" {
  kubernetes {
    host  = "https://${google_container_cluster.autopilot.endpoint}"
    token = data.google_client_config.default.access_token
    cluster_ca_certificate = base64decode(
      google_container_cluster.autopilot.master_auth[0].cluster_ca_certificate
    )
  }
}
