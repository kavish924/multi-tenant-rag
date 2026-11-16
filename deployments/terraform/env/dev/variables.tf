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
  description = "Cloud SQL machine tier; small in dev"
  default     = "db-custom-1-3840"
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
  default = 3
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
