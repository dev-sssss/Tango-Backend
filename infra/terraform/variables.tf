variable "project" {
  description = "Name prefix for every resource."
  type        = string
  default     = "tango"
}

variable "environment" {
  description = "Environment name (this stack is non-prod)."
  type        = string
  default     = "nonprod"
}

variable "region" {
  description = "AWS region. Mumbai is closest to players in India."
  type        = string
  default     = "ap-south-1"
}

variable "instance_type" {
  description = "Graviton (ARM) instance; t4g.medium = 2 vCPU, 4 GB."
  type        = string
  default     = "t4g.medium"
}

variable "root_volume_gb" {
  type    = number
  default = 30
}

variable "vpc_cidr" {
  type    = string
  default = "10.40.0.0/16"
}

variable "domain_name" {
  description = "Optional hostname (e.g. tango-nonprod.example.com). Empty = clients use the Elastic IP directly (HTTPS with a Let's Encrypt IP certificate)."
  type        = string
  default     = ""
}

variable "route53_zone_id" {
  description = "Route 53 hosted zone for domain_name. Empty = no DNS record is created."
  type        = string
  default     = ""
}

variable "backup_retention_days" {
  type    = number
  default = 7
}
