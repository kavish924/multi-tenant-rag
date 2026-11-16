# Foundation for one project: APIs, network, Artifact Registry, Cloud Build
# service account + triggers. No Cloud Workstations: a cluster bills ~$0.20/h
# even when idle; development uses Cloud Shell (docs/gcp-environment.md §3).
# Applied once from Cloud Shell (docs/gcp-environment.md §11).

terraform {
  required_version = ">= 1.6"
  backend "gcs" {} # bucket/prefix passed with -backend-config at init
  required_providers {
    google      = { source = "hashicorp/google", version = ">= 6.0, < 8.0" }
    google-beta = { source = "hashicorp/google-beta", version = ">= 6.0, < 8.0" }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

provider "google-beta" {
  project = var.project_id
  region  = var.region
}

locals {
  apis = [
    "aiplatform.googleapis.com",
    "artifactregistry.googleapis.com",
    "cloudbuild.googleapis.com",
    "cloudtrace.googleapis.com",
    "compute.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "logging.googleapis.com",
    "monitoring.googleapis.com",
    "run.googleapis.com",
    "secretmanager.googleapis.com",
    "servicenetworking.googleapis.com",
    "sqladmin.googleapis.com",
    "storage.googleapis.com",
  ]
}

resource "google_project_service" "apis" {
  for_each           = toset(local.apis)
  service            = each.value
  disable_on_destroy = false
}

# ---------------------------------------------------------------- network

resource "google_compute_network" "vpc" {
  name                    = "${var.env}-vpc"
  auto_create_subnetworks = false
  depends_on              = [google_project_service.apis]
}

resource "google_compute_subnetwork" "main" {
  name                     = "${var.env}-main"
  region                   = var.region
  network                  = google_compute_network.vpc.id
  ip_cidr_range            = var.subnet_cidr
  private_ip_google_access = true
}

# Private services access: Cloud SQL gets a private IP in this VPC.
resource "google_compute_global_address" "psa" {
  name          = "${var.env}-psa"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  prefix_length = 16
  network       = google_compute_network.vpc.id
}

resource "google_service_networking_connection" "psa" {
  network                 = google_compute_network.vpc.id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.psa.name]
}

# -------------------------------------------------------- artifact registry

resource "google_artifact_registry_repository" "app" {
  repository_id = "app"
  location      = var.region
  format        = "DOCKER"
  depends_on    = [google_project_service.apis]

  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"
    most_recent_versions {
      keep_count = 5 # stay inside the Artifact Registry free storage
    }
  }
}

# ------------------------------------------------------------- cloud build

resource "google_service_account" "build" {
  account_id   = "sa-build"
  display_name = "Cloud Build (CI/CD)"
}

resource "google_project_iam_member" "build" {
  for_each = toset([
    "roles/artifactregistry.writer",
    "roles/logging.logWriter",
    "roles/run.developer",
  ])
  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${google_service_account.build.email}"
}

# The GitHub repository must be connected to Cloud Build once in the console
# (Cloud Build → Triggers → Connect repository) before these apply.
resource "google_cloudbuild_trigger" "branches" {
  name            = "branches"
  description     = "Checks on every branch except main"
  filename        = "cloudbuild.yaml"
  service_account = google_service_account.build.id
  substitutions   = { _REGION = var.region, _DEPLOY = "false" }

  github {
    owner = var.github_owner
    name  = var.github_repo
    push {
      branch       = "^main$"
      invert_regex = true
    }
  }
}

resource "google_cloudbuild_trigger" "main" {
  name            = "main"
  description     = "Checks, push images, migrate and deploy ${var.env}"
  filename        = "cloudbuild.yaml"
  service_account = google_service_account.build.id
  substitutions   = { _REGION = var.region, _DEPLOY = "true" }

  github {
    owner = var.github_owner
    name  = var.github_repo
    push {
      branch = "^main$"
    }
  }
}
