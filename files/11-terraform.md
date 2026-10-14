# Chapter 11 --- Infrastructure as Code with Terraform

Back in Chapter 9 we created real infrastructure with a string of imperative commands: `gcloud container clusters create-auto`, `gcloud storage buckets create`, `kubectl create secret`, even a `helm install` for NATS. Each one worked. Together they are a problem. Nobody reviewed them. They live in a terminal's scrollback, not in version control. If a teammate needs to stand up a staging environment, they have to remember the exact flags you used --- and if they get one wrong, staging quietly differs from production in a way no one notices until it breaks. Six months from now, no one will be able to say with confidence what the production project actually contains, because the source of truth is "whatever commands happened to be run." This is configuration drift, and it is how cloud environments rot.

Infrastructure as code fixes this by treating your cloud resources the way we already treat application code: as declarative files, in Git, reviewed in pull requests, applied by a tool that reconciles reality with the declaration. This chapter rebuilds Snapvault's infrastructure --- the bucket, the registry, the cluster, the service accounts, the database, the CI trust relationship --- as **Terraform**, so the entire environment is reproducible from a directory of text files.

## What Terraform is

Terraform is a tool for declaring infrastructure in a language called HCL and then making a cloud match that declaration. You describe the resources you want --- a bucket, a cluster, a database --- and Terraform figures out what to create, change, or destroy to get there. Three ideas make it work.

**Providers** are plugins that teach Terraform how to talk to a specific API. We'll use the `google` provider for Google Cloud. **Resources** are the things you declare --- `google_storage_bucket`, `google_container_cluster` --- each a typed block with the settings that API accepts. And **state** is Terraform's record of what it has already created, mapping your declarations to real resource IDs so it knows, on the next run, what already exists and what changed. The workflow is always the same: `terraform plan` shows you the diff between your files and reality, and `terraform apply` enacts it. Nothing happens to your cloud that you didn't see in a plan first.

## Project layout and the provider

Create an `infra/` directory at the root of the repo. Start with `providers.tf`, which pins the provider and --- importantly --- stores Terraform's state remotely so it isn't trapped on one laptop:

``` hcl
terraform {
  required_version = ">= 1.9"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }

  backend "gcs" {
    bucket = "snapvault-tfstate"
    prefix = "prod"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}
```

The `backend "gcs"` block keeps state in a Cloud Storage bucket rather than a local file. This matters more than it looks. State is the source of truth for what Terraform manages; if it lives on your machine, only you can run Terraform, and losing the file means Terraform forgets it owns everything. A GCS backend puts state in one shared, versioned place and --- because the GCS backend supports locking --- prevents two people from applying at the same time and corrupting it. You create this one bucket by hand, once, before anything else; it's the bootstrap exception to "everything in code."

Declare the inputs in `variables.tf`:

``` hcl
variable "project_id" {
  type        = string
  description = "The Google Cloud project ID."
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "forgejo_repo" {
  type        = string
  description = "owner/repo on Codeberg allowed to deploy via Workload Identity."
}

variable "forgejo_issuer" {
  type        = string
  default     = "https://codeberg.org/api/actions"
  description = "OIDC issuer of your Forgejo instance (instance URL + /api/actions)."
}
```

and set them in `terraform.tfvars` (which holds non-secret values and is fine to commit, or to keep per-environment):

``` hcl
project_id   = "snapvault-prod-12345"
region       = "us-central1"
forgejo_repo = "your-org/snapvault"
```

## Storage and the registry

Two of the simplest resources first: the bucket that holds uploaded images, and the Artifact Registry repository that holds our container images. Create `storage.tf`:

``` hcl
resource "google_storage_bucket" "uploads" {
  name                        = "snapvault-uploads-prod"
  location                    = var.region
  uniform_bucket_level_access = true
  force_destroy               = false

  lifecycle_rule {
    condition { age = 365 }
    action { type = "SetStorageClass"
      storage_class = "NEARLINE" }
  }
}

resource "google_artifact_registry_repository" "snapvault" {
  repository_id = "snapvault"
  location      = var.region
  format        = "DOCKER"
  description   = "Snapvault service container images"
}
```

`uniform_bucket_level_access` turns off legacy per-object ACLs and forces all access through IAM, which is the modern, auditable model. The `lifecycle_rule` is a small taste of what declarative infrastructure makes easy: a policy to move originals to cheaper Nearline storage after a year, expressed once and enforced forever. The Artifact Registry repository is the one our Chapter 8 pipeline pushes to --- the registry path `us-central1-docker.pkg.dev/PROJECT_ID/snapvault/...` is exactly `location-docker.pkg.dev/project/repository_id`.

## The cluster

The GKE Autopilot cluster from Chapter 9, now as code. Create `gke.tf`:

``` hcl
resource "google_container_cluster" "snapvault" {
  name             = "snapvault"
  location         = var.region
  enable_autopilot = true

  # Autopilot manages node pools; we only declare the cluster.
  release_channel {
    channel = "REGULAR"
  }

  ip_allocation_policy {} # required for Autopilot (VPC-native)
}
```

`enable_autopilot = true` is the whole difference between this and a Standard cluster --- Google manages the nodes, and we declare nothing about machine types or counts. The `release_channel` opts the cluster into automatic, staged Kubernetes upgrades. After `apply`, you fetch credentials exactly as before with `gcloud container clusters get-credentials snapvault --region=us-central1`. Terraform provisions the cluster; `kubectl` and the manifests from Chapter 9 deploy into it. Keeping that boundary clean --- Terraform owns infrastructure, Kubernetes manifests own workloads --- keeps both layers comprehensible.

## Service accounts and workload identity

Snapvault's Pods need to read and write the GCS bucket, and they should do it without a key file. Back in Chapter 5 we relied on Application Default Credentials; the mechanism that supplies them in GKE is **Workload Identity**, which lets a Kubernetes service account act as a Google service account. Declare the Google service account, grant it bucket access, and bind it to the in-cluster `snapvault` service account from our Chapter 9 manifests. Create `iam.tf`:

``` hcl
resource "google_service_account" "runtime" {
  account_id   = "snapvault-runtime"
  display_name = "Snapvault runtime service account"
}

resource "google_storage_bucket_iam_member" "runtime_uploads" {
  bucket = google_storage_bucket.uploads.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.runtime.email}"
}

# Let the Kubernetes SA snapvault/snapvault impersonate the Google SA.
resource "google_service_account_iam_member" "workload_identity" {
  service_account_id = google_service_account.runtime.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[snapvault/snapvault]"
}
```

The last resource is the magic that connects this chapter to the last one. `${project}.svc.id.goog[namespace/ksa]` names the Kubernetes service account `snapvault` in the `snapvault` namespace --- the `serviceAccountName: snapvault` we put on every Pod. Granting it `workloadIdentityUser` on the Google service account lets those Pods authenticate to Google Cloud as `snapvault-runtime`, with no key anywhere. You finish the link with one annotation on the Kubernetes side:

``` bash
$ kubectl annotate serviceaccount snapvault -n snapvault \
    iam.gke.io/gcp-service-account=snapvault-runtime@PROJECT_ID.iam.gserviceaccount.com
```

Now the images and thumbnails Pods reach Cloud Storage with the identity Terraform created --- the keyless access we promised in Chapter 5 finally fully wired.

## The CI trust relationship

Chapter 8's pipeline authenticated to Google Cloud with Workload Identity Federation, and we promised the one-time setup would come later. Here it is, as code. A workload identity *pool* and a *provider* establish trust between the OIDC tokens your Forgejo instance issues and a Google service account, scoped to exactly your repository. Create `cicd-iam.tf`:

``` hcl
resource "google_service_account" "deployer" {
  account_id   = "snapvault-deployer"
  display_name = "CI/CD deployer"
}

resource "google_project_iam_member" "deployer_artifacts" {
  project = var.project_id
  role    = "roles/artifactregistry.writer"
  member  = "serviceAccount:${google_service_account.deployer.email}"
}

resource "google_iam_workload_identity_pool" "forgejo" {
  workload_identity_pool_id = "forgejo-pool"
  display_name              = "Forgejo Actions (Codeberg)"
}

resource "google_iam_workload_identity_pool_provider" "forgejo" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.forgejo.workload_identity_pool_id
  workload_identity_pool_provider_id = "forgejo-provider"

  attribute_mapping = {
    "google.subject"       = "assertion.sub"
    "attribute.repository" = "assertion.repository"
  }
  # Only tokens whose repository claim matches our repo are accepted.
  attribute_condition = "assertion.repository == '${var.forgejo_repo}'"

  oidc {
    issuer_uri = var.forgejo_issuer # e.g. https://codeberg.org/api/actions
  }
}

# Let workflows from our repo impersonate the deployer SA.
resource "google_service_account_iam_member" "forgejo_deployer" {
  service_account_id = google_service_account.deployer.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.forgejo.name}/attribute.repository/${var.forgejo_repo}"
}
```

::: warning
The `attribute_condition` is not optional security theatre --- it is the line that stops *any* repository on your Forgejo instance from minting a token your project will trust. The issuer (`https://codeberg.org/api/actions`) is shared by every repository on Codeberg, so without a condition pinning the `repository` claim to your repo, any Codeberg user's workflow could request a token your project would accept. Always scope the provider to your repository (and, by also asserting on the `sub` claim, ideally your branch), as the condition above does.
:::

With this applied, the `workload_identity_provider` and `service_account` values your Chapter 8 workflow references are outputs you can print --- close the loop by exposing them in `outputs.tf`:

``` hcl
output "wif_provider" {
  value = google_iam_workload_identity_pool_provider.forgejo.name
}

output "deployer_sa" {
  value = google_service_account.deployer.email
}
```

## The database

The feed service needs PostgreSQL. We argued in Chapter 9 for a managed instance rather than an in-cluster StatefulSet; **Cloud SQL** is that instance, and it's a resource like any other. Create `database.tf`:

``` hcl
resource "google_sql_database_instance" "feed" {
  name             = "snapvault-feed"
  database_version = "POSTGRES_16"
  region           = var.region

  settings {
    tier              = "db-custom-1-3840"
    availability_type = "ZONAL" # REGIONAL for HA in production
    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
    }
    # A public IP with no authorized networks: nothing can connect directly.
    # Pods connect through the Cloud SQL Auth Proxy (below).
    ip_configuration {
      ipv4_enabled = true
    }
  }

  deletion_protection = true
}

resource "google_sql_database" "snapvault" {
  name     = "snapvault"
  instance = google_sql_database_instance.feed.name
}

# Let the runtime identity open connections through the Auth Proxy.
resource "google_project_iam_member" "runtime_cloudsql" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.runtime.email}"
}

output "cloudsql_connection_name" {
  value = google_sql_database_instance.feed.connection_name
}
```

`point_in_time_recovery_enabled` and automated backups are exactly the operational properties we did *not* want to build ourselves --- declaring `enabled = true` is the entire reason to use a managed database. `deletion_protection` guards against an accidental `terraform destroy` taking the production database with it.

A database is only useful if the feed Pods can reach it, and the instance above deliberately accepts no direct connections. The standard way in from GKE is the **Cloud SQL Auth Proxy**: a small container running next to the feed service in the same Pod. It authenticates as the Pod's Google identity (hence `roles/cloudsql.client` on the runtime account), opens an encrypted connection to the instance, and listens on `127.0.0.1:5432`. The feed service just connects to localhost. Add it to the feed Deployment as a *native sidecar* --- an init container with `restartPolicy: Always`, which starts before the main container and runs alongside it:

``` yaml
    spec:
      serviceAccountName: snapvault
      initContainers:
        - name: cloud-sql-proxy
          image: gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.14.1
          restartPolicy: Always
          args:
            - --port=5432
            - PROJECT_ID:us-central1:snapvault-feed   # cloudsql_connection_name
          resources:
            requests: { cpu: 100m, memory: 64Mi }
      containers:
        - name: feed
          # ... as before
```

(The alternative is a private IP, which needs VPC peering between your network and Google's service network --- more moving parts, and worth it mainly when you have many clients or a policy against public endpoints.)

The database also needs a user. Create it out of band, so the password never enters Terraform state:

``` bash
$ gcloud sql users create snapvault --instance=snapvault-feed --password="$DB_PASSWORD"
```

The connection string, with that password, is a secret. Because the proxy listens inside the Pod, it reads `postgres://snapvault:PASSWORD@127.0.0.1:5432/snapvault?sslmode=disable` --- `sslmode=disable` covers only the loopback hop, and the proxy encrypts everything beyond it. Store it in **Secret Manager** so the cluster can pull it at runtime (Chapter 14) instead of carrying a hand-created Kubernetes Secret:

``` hcl
resource "google_secret_manager_secret" "database_url" {
  secret_id = "snapvault-database-url"
  replication {
    auto {}
  }
}
```

You write the secret's value out of band too --- Terraform should declare that the secret *exists*, not bake the password into state. MongoDB Atlas is provisioned the same way through its own Terraform provider (`mongodb/mongodbatlas`); it's outside our scope here, but the pattern is identical: declare the cluster, capture the connection string as a Secret Manager secret.

## The workflow

With the files in place, the loop is three commands. Initialize to download the provider and configure the backend, review the plan, then apply:

``` bash
$ cd infra
$ terraform init
$ terraform plan
$ terraform apply
```

`plan` prints every resource Terraform will create, change, or destroy --- read it. This is the review step that imperative `gcloud` commands never had; a destructive change shows up here in red before it touches anything. `apply` re-shows the plan and waits for you to type `yes`. From then on, every infrastructure change is a pull request that edits these files, reviewed by a human, and applied --- ideally by CI, with the same Workload Identity trust we set up for image builds. Your cloud environment is now reproducible: a second team could stand up an identical staging project by pointing these files at a new `project_id`.

::: tip
As the file count grows, group related resources into **modules** --- a `module "gke"`, a `module "data"` --- each a reusable directory with its own variables and outputs. Don't reach for modules on day one, though. A flat set of `.tf` files is easier to read until the repetition across environments actually hurts; premature modularization is as much a trap in Terraform as premature microservices were in Chapter 1.
:::

### Summary

-   Imperative `gcloud`/`kubectl` commands aren't reviewable or reproducible and lead to **configuration drift**; infrastructure as code puts your cloud in version control instead.
-   **Terraform** declares resources in HCL via **providers**, tracks what it created in **state**, and reconciles the two with `plan` (preview the diff) and `apply` (enact it).
-   Keep Terraform **state in a GCS backend**, not on a laptop --- it's the shared source of truth and supports locking against concurrent applies.
-   Snapvault's whole environment becomes code: the **uploads bucket**, the **Artifact Registry** repo, the **GKE Autopilot cluster**, **Cloud SQL**, service accounts, and Secret Manager secrets.
-   **Workload Identity** binds the Kubernetes `snapvault` service account to a Google service account, giving Pods keyless Cloud Storage access --- the mechanism behind Chapter 5's Application Default Credentials.
-   The **Workload Identity Federation** pool and provider establish keyless CI trust scoped to your repo with an `attribute_condition` --- the one-time setup Chapter 8 assumed.
-   Pods reach **Cloud SQL** through the **Cloud SQL Auth Proxy** sidecar, authenticated by workload identity (`roles/cloudsql.client`), so the instance accepts no direct connections.
-   Declare that **secrets exist** in Secret Manager and write their values out of band; never bake passwords into Terraform state.
-   Keep a clean boundary: Terraform owns infrastructure, Kubernetes manifests own workloads. Reach for **modules** only when repetition across environments actually hurts.

The infrastructure is now reproducible, and the manifests from Chapter 9 are ready to deploy onto it. But we're still deploying them the way Chapter 9 did --- `kubectl apply` from a laptop, an imperative act that leaves no trace and lets the cluster drift from any record of what should be running. That's the same problem this chapter just solved for infrastructure, still unsolved for deployment. The next chapter fixes it with GitOps: a Git repository as the source of truth for the cluster, and ArgoCD continuously reconciling reality to match it.
