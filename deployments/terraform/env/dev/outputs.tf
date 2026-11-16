output "api_url" {
  value = google_cloud_run_v2_service.api.uri
}

output "sql_connection_name" {
  value = google_sql_database_instance.main.connection_name
}

output "documents_bucket" {
  value = google_storage_bucket.documents.name
}

output "db_users" {
  description = "Cloud SQL IAM database user per runtime service account"
  value       = local.db_user
}
