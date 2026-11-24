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

resource "random_password" "pg" {
  length  = 24
  special = false
}

resource "google_sql_user" "app" {
  name     = "snapvault"
  instance = google_sql_database_instance.postgres.name
  password = random_password.pg.result
}

# Let the runtime identity connect through the Cloud SQL Auth Proxy.
resource "google_project_iam_member" "runtime_cloudsql" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.runtime.email}"
}

# The feed service reads DATABASE_URL from Secret Manager (mounted by the CSI
# driver). Keeping the connection string here means it never lands in git.
resource "google_secret_manager_secret" "database_url" {
  secret_id = "snapvault-database-url"
  replication {
    auto {}
  }
}

# The DSN points at the Auth Proxy sidecar on localhost. sslmode=disable only
# covers the hop inside the Pod; the proxy encrypts the connection to Cloud SQL.
resource "google_secret_manager_secret_version" "database_url" {
  secret = google_secret_manager_secret.database_url.id
  secret_data = format(
    "postgres://%s:%s@127.0.0.1:5432/%s?sslmode=disable",
    google_sql_user.app.name,
    random_password.pg.result,
    google_sql_database.feed.name,
  )
}

resource "google_secret_manager_secret_iam_member" "runtime_db" {
  secret_id = google_secret_manager_secret.database_url.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runtime.email}"
}

# NOTE: MongoDB (metadata service) is not a managed GCP product. Run it via
# MongoDB Atlas or a self-managed StatefulSet, and store its MONGO_URI in
# Secret Manager the same way as DATABASE_URL above.
