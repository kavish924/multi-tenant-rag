variable "project_id" {
  type = string
}

variable "env" {
  type    = string
  default = "dev"
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "db_tier" {
  type        = string
  description = "Cloud SQL tier. Shared-core db-f1-micro (~$9/month) fits the free-trial budget; use db-custom-* for test/prod"
  default     = "db-f1-micro"
}

variable "db_availability" {
  type        = string
  description = "ZONAL in dev, REGIONAL in prod"
  default     = "ZONAL"
}

variable "db_pitr" {
  type    = bool
  default = false
}

variable "deletion_protection" {
  type    = bool
  default = true
}

variable "bucket_versioning" {
  type        = bool
  description = "Object versioning for accidental-deletion recovery (spec 013)"
  default     = false
}

variable "api_min_instances" {
  type    = number
  default = 0
}

variable "api_max_instances" {
  type    = number
  default = 2
}

variable "api_public" {
  type        = bool
  description = "Allow unauthenticated invocation (the API does its own JWT auth)"
  default     = true
}

variable "placeholder_image" {
  type        = string
  description = "Image used only on first create; Cloud Build deploys the real one"
  default     = "us-docker.pkg.dev/cloudrun/container/hello"
}

variable "otel_collector_image" {
  type        = string
  description = "OpenTelemetry collector sidecar image (contrib build: googlecloud + googlemanagedprometheus exporters)"
  default     = "otel/opentelemetry-collector-contrib:0.121.0"
}

variable "trace_sample_ratio" {
  type        = number
  description = "Head-sampling ratio for new traces: 1.0 in dev, 0.1 in prod (spec 010)"
  default     = 1.0
}
