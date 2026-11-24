# Cloud SQL for PostgreSQL backs the feed service.
resource "google_sql_database_instance" "postgres" {
  name             = "snapvault-pg"
  database_version = "POSTGRES_16"
  region           = var.region

  settings {
    tier = "db-custom-1-3840"

    # Nightly backups plus point-in-time recovery (from the write-ahead log),
    # the main reason to use a managed database rather than run our own.
    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
    }

    # The instance gets a public IP but no authorized networks, so nothing can
    # connect to it directly. The feed Pods reach it through the Cloud SQL Auth
    # Proxy sidecar, which authenticates with the runtime service account
    # (roles/cloudsql.client below) and encrypts the connection. A private IP
    # with VPC peering is the stricter alternative.
    ip_configuration {
      ipv4_enabled = true
    }
  }

  deletion_protection = true
}

resource "google_sql_database" "feed" {
  name     = "snapvault"
  instance = google_sql_database_instance.postgres.name
}

# The database user and its password are created out of band, so the password
# never enters Terraform state (anyone who can read the state could read it):
#
#   gcloud sql users create snapvault --instance=snapvault-pg --password="$DB_PASSWORD"
#   printf 'postgres://snapvault:%s@127.0.0.1:5432/snapvault?sslmode=disable' "$DB_PASSWORD" |
#     gcloud secrets versions add snapvault-database-url --data-file=-
#
# The DSN points at the Auth Proxy sidecar on localhost; sslmode=disable only
# covers the hop inside the Pod, and the proxy encrypts the rest.
#
# Earlier versions of this file generated the password here. These removed
# blocks make Terraform forget those resources without destroying them, so an
# existing user and secret version survive the upgrade. Their values are still
# in old state snapshots, so rotate the password after applying.
removed {
  from = random_password.pg
  lifecycle { destroy = false }
}

removed {
  from = google_sql_user.app
  lifecycle { destroy = false }
}

removed {
  from = google_secret_manager_secret_version.database_url
  lifecycle { destroy = false }
}

# Let the runtime identity connect through the Cloud SQL Auth Proxy.
resource "google_project_iam_member" "runtime_cloudsql" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.runtime.email}"
}

# The feed service reads DATABASE_URL from Secret Manager, mounted into its Pod
# by GKE's Secret Manager add-on (secrets.tf). Terraform declares that the
# secret exists; its value is added out of band (see the commands above).
resource "google_secret_manager_secret" "database_url" {
  secret_id = "snapvault-database-url"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_iam_member" "runtime_db" {
  secret_id = google_secret_manager_secret.database_url.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runtime.email}"
}
