# Runtime for one environment: Cloud SQL, documents bucket, runtime service
# accounts and their IAM database users, the JWT secret, the Cloud Run `api`
# service and `migrate` job. Requires bootstrap to have been applied in the
# same project. Cloud Build owns the deployed images after the first apply.

terraform {
  required_version = ">= 1.6"
  backend "gcs" {}
  required_providers {
    google = { source = "hashicorp/google", version = ">= 6.0, < 8.0" }
    random = { source = "hashicorp/random", version = ">= 3.5" }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

locals {
  runtime = ["api", "worker", "relay", "migrate"]
  # Cloud SQL IAM database user name for a service account.
  db_user = { for k, sa in google_service_account.runtime : k => trimsuffix(sa.email, ".gserviceaccount.com") }
}

data "google_compute_network" "vpc" {
  name = "${var.env}-vpc"
}

data "google_compute_subnetwork" "main" {
  name   = "${var.env}-main"
  region = var.region
}

data "google_service_account" "build" {
  account_id = "sa-build"
}

# ---------------------------------------------------------- service accounts

resource "google_service_account" "runtime" {
  for_each     = toset(local.runtime)
  account_id   = "sa-${each.key}"
  display_name = "Runtime: ${each.key}"
}

resource "google_project_iam_member" "sql" {
  for_each = {
    for pair in setproduct(local.runtime, ["roles/cloudsql.client", "roles/cloudsql.instanceUser"]) :
    "${pair[0]}-${pair[1]}" => { sa = pair[0], role = pair[1] }
  }
  project = var.project_id
  role    = each.value.role
  member  = "serviceAccount:${google_service_account.runtime[each.value.sa].email}"
}

resource "google_project_iam_member" "vertex" {
  for_each = toset(["api", "worker"])
  project  = var.project_id
  role     = "roles/aiplatform.user"
  member   = "serviceAccount:${google_service_account.runtime[each.key].email}"
}

# sa-api signs Cloud Storage V4 URLs through IAM signBlob on itself.
resource "google_service_account_iam_member" "api_self_sign" {
  service_account_id = google_service_account.runtime["api"].name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:${google_service_account.runtime["api"].email}"
}

# Cloud Build deploys as these service accounts.
resource "google_service_account_iam_member" "build_act_as" {
  for_each           = google_service_account.runtime
  service_account_id = each.value.name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${data.google_service_account.build.email}"
}

# ----------------------------------------------------------------- cloud sql

resource "google_sql_database_instance" "main" {
  name                = "${var.env}-pg"
  region              = var.region
  database_version    = "POSTGRES_17"
  deletion_protection = var.deletion_protection

  settings {
    edition           = "ENTERPRISE"
    tier              = var.db_tier
    availability_type = var.db_availability
    disk_autoresize   = true

    ip_configuration {
      ipv4_enabled    = false
      private_network = data.google_compute_network.vpc.id
      ssl_mode        = "ENCRYPTED_ONLY"
    }

    database_flags {
      name  = "cloudsql.iam_authentication"
      value = "on"
    }

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = var.db_pitr
    }

    insights_config {
      query_insights_enabled = true
    }
  }
}

resource "google_sql_database" "app" {
  name     = "app"
  instance = google_sql_database_instance.main.name
}

resource "google_sql_user" "iam" {
  for_each = google_service_account.runtime
  name     = local.db_user[each.key]
  instance = google_sql_database_instance.main.name
  type     = "CLOUD_IAM_SERVICE_ACCOUNT"
}

# ------------------------------------------------------------ cloud storage

resource "google_storage_bucket" "documents" {
  name                        = "${var.project_id}-documents"
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false

  versioning {
    enabled = var.bucket_versioning
  }
}

resource "google_storage_bucket_iam_member" "documents" {
  for_each = toset(["api", "worker"])
  bucket   = google_storage_bucket.documents.name
  role     = "roles/storage.objectAdmin"
  member   = "serviceAccount:${google_service_account.runtime[each.key].email}"
}

# ------------------------------------------------------------------ secrets

# The key is generated here, so it lives in Terraform state: keep the state
# bucket restricted to operators. Rotate with `terraform apply -replace`.
resource "random_password" "jwt" {
  length  = 64
  special = false
}

resource "google_secret_manager_secret" "jwt" {
  secret_id = "jwt-signing-key"
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "jwt" {
  secret      = google_secret_manager_secret.jwt.id
  secret_data = random_password.jwt.result
}

resource "google_secret_manager_secret_iam_member" "jwt_api" {
  secret_id = google_secret_manager_secret.jwt.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runtime["api"].email}"
}

# ---------------------------------------------------------------- cloud run

resource "google_cloud_run_v2_service" "api" {
  name                = "api"
  location            = var.region
  ingress             = "INGRESS_TRAFFIC_ALL"
  deletion_protection = false

  template {
    service_account                  = google_service_account.runtime["api"].email
    timeout                          = "900s" # long enough for SSE answers
    max_instance_request_concurrency = 80

    scaling {
      min_instance_count = var.api_min_instances
      max_instance_count = var.api_max_instances
    }

    vpc_access {
      egress = "PRIVATE_RANGES_ONLY"
      network_interfaces {
        network    = data.google_compute_network.vpc.id
        subnetwork = data.google_compute_subnetwork.main.id
      }
    }

    containers {
      image = var.placeholder_image
      ports {
        container_port = 8080
      }
      resources {
        limits = { cpu = "1", memory = "512Mi" }
      }
      env {
        name  = "CLOUDSQL_INSTANCE"
        value = google_sql_database_instance.main.connection_name
      }
      env {
        name  = "DB_USER"
        value = local.db_user["api"]
      }
      env {
        name  = "DB_NAME"
        value = google_sql_database.app.name
      }
      env {
        name = "JWT_SIGNING_KEY"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.jwt.secret_id
            version = "latest"
          }
        }
      }
    }
  }

  # Cloud Build updates the image on every main build.
  lifecycle {
    ignore_changes = [template[0].containers[0].image, client, client_version]
  }

  depends_on = [
    google_secret_manager_secret_version.jwt,
    google_secret_manager_secret_iam_member.jwt_api,
  ]
}

# The API authenticates every request itself (JWT); Cloud Run IAM is not the gate.
resource "google_cloud_run_v2_service_iam_member" "api_public" {
  count    = var.api_public ? 1 : 0
  name     = google_cloud_run_v2_service.api.name
  location = var.region
  role     = "roles/run.invoker"
  member   = "allUsers"
}

resource "google_cloud_run_v2_job" "migrate" {
  name                = "migrate"
  location            = var.region
  deletion_protection = false

  template {
    task_count = 1
    template {
      service_account = google_service_account.runtime["migrate"].email
      timeout         = "600s"
      max_retries     = 0

      vpc_access {
        egress = "PRIVATE_RANGES_ONLY"
        network_interfaces {
          network    = data.google_compute_network.vpc.id
          subnetwork = data.google_compute_subnetwork.main.id
        }
      }

      containers {
        image = var.placeholder_image
        env {
          name  = "CLOUDSQL_INSTANCE"
          value = google_sql_database_instance.main.connection_name
        }
        env {
          name  = "DB_USER"
          value = local.db_user["migrate"]
        }
        env {
          name  = "DB_NAME"
          value = google_sql_database.app.name
        }
        env {
          name  = "APP_DB_LOGIN_ROLES"
          value = join(",", [for k in ["api", "worker", "relay"] : local.db_user[k]])
        }
      }
    }
  }

  lifecycle {
    ignore_changes = [template[0].template[0].containers[0].image, client, client_version]
  }
}
