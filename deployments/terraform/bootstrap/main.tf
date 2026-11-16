# Foundation for one project: APIs, network, Artifact Registry, Cloud Build
# service account + triggers, and the Cloud Workstations cluster/config.
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
    "workstations.googleapis.com",
  ]
  workstation_image = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.app.repository_id}/workstation:latest"
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
      keep_count = 20
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

# -------------------------------------------------------- cloud workstations

resource "google_service_account" "workstation" {
  account_id   = "sa-workstation"
  display_name = "Cloud Workstations VM"
}

resource "google_artifact_registry_repository_iam_member" "workstation_pull" {
  location   = google_artifact_registry_repository.app.location
  repository = google_artifact_registry_repository.app.name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.workstation.email}"
}

resource "google_workstations_workstation_cluster" "dev" {
  provider               = google-beta
  workstation_cluster_id = "${var.env}-cluster"
  location               = var.region
  network                = google_compute_network.vpc.id
  subnetwork             = google_compute_subnetwork.main.id
  depends_on             = [google_project_service.apis]
}

resource "google_workstations_workstation_config" "go" {
  provider               = google-beta
  workstation_config_id  = "go-dev"
  workstation_cluster_id = google_workstations_workstation_cluster.dev.workstation_cluster_id
  location               = var.region

  idle_timeout    = "1800s"  # stop billing ~30 min after the last activity
  running_timeout = "43200s" # hard cap: 12 h per session

  host {
    gce_instance {
      machine_type                = var.workstation_machine_type
      boot_disk_size_gb           = 50
      service_account             = google_service_account.workstation.email
      disable_public_ip_addresses = false # needs internet for go modules / images
    }
  }

  persistent_directories {
    mount_path = "/home"
    gce_pd {
      size_gb        = 200
      fs_type        = "ext4"
      reclaim_policy = "RETAIN"
    }
  }

  container {
    image = local.workstation_image
  }
}

# Lets the developer create and use their own workstation from the config.
resource "google_workstations_workstation_config_iam_member" "developer" {
  provider               = google-beta
  for_each               = toset(var.developers)
  location               = var.region
  workstation_cluster_id = google_workstations_workstation_cluster.dev.workstation_cluster_id
  workstation_config_id  = google_workstations_workstation_config.go.workstation_config_id
  role                   = "roles/workstations.workstationCreator"
  member                 = each.value
}
