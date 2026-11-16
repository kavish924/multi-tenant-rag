variable "project_id" {
  type        = string
  description = "GCP project for this environment, e.g. <prefix>-dev"
}

variable "env" {
  type        = string
  description = "Environment name: dev, test or prod"
  default     = "dev"
}

variable "region" {
  type        = string
  description = "Single region for Cloud SQL, Cloud Run, Cloud Storage and Vertex AI"
  default     = "us-central1"
}

variable "subnet_cidr" {
  type    = string
  default = "10.10.0.0/20"
}

variable "github_owner" {
  type        = string
  description = "GitHub owner of the connected repository"
}

variable "github_repo" {
  type        = string
  description = "GitHub repository name"
}
