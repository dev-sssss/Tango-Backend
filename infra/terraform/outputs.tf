output "instance_id" {
  value = aws_instance.server.id
}

output "public_ip" {
  value = aws_eip.server.public_ip
}

output "hostname" {
  description = "Public hostname or IP of the server (the TLS certificate is issued for it)."
  value       = local.hostname
}

output "api_url" {
  value = "https://${local.hostname}"
}

output "ecr_repository_url" {
  value = aws_ecr_repository.server.repository_url
}

output "ops_bucket" {
  value = aws_s3_bucket.ops.bucket
}

output "param_path" {
  value = local.param_path
}

output "region" {
  value = var.region
}
