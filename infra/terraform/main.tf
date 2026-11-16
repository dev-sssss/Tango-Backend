# Without a domain the server is reached by its Elastic IP; Let's Encrypt
# issues short-lived (6-day, auto-renewed) certificates for IP addresses.
locals {
  name        = "${var.project}-${var.environment}"
  param_path  = "/${var.project}/${var.environment}"
  hostname    = var.domain_name != "" ? var.domain_name : aws_eip.server.public_ip
  bucket_name = "${local.name}-${data.aws_caller_identity.me.account_id}-${var.region}"
}

data "aws_caller_identity" "me" {}

data "aws_availability_zones" "available" {
  state = "available"
}

# Ubuntu 24.04 LTS for ARM (Graviton), latest Canonical image.
data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"]
  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*"]
  }
  filter {
    name   = "architecture"
    values = ["arm64"]
  }
}

# ---------------------------------------------------------------- network
# One small public subnet; no NAT gateway (keeps the bill low).

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = local.name }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = local.name }
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, 1)
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = false
  tags                    = { Name = "${local.name}-public" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }
  tags = { Name = "${local.name}-public" }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

# Only HTTP (for the TLS certificate challenge and redirect) and HTTPS are
# open. There is no SSH: the server is managed through AWS Systems Manager.
resource "aws_security_group" "server" {
  name        = "${local.name}-server"
  description = "Tango game server: HTTPS and HTTP only"
  vpc_id      = aws_vpc.main.id

  ingress {
    description      = "HTTP (ACME challenge, redirect)"
    from_port        = 80
    to_port          = 80
    protocol         = "tcp"
    cidr_blocks      = ["0.0.0.0/0"]
    ipv6_cidr_blocks = ["::/0"]
  }
  ingress {
    description      = "HTTPS API and WebSockets"
    from_port        = 443
    to_port          = 443
    protocol         = "tcp"
    cidr_blocks      = ["0.0.0.0/0"]
    ipv6_cidr_blocks = ["::/0"]
  }
  egress {
    from_port        = 0
    to_port          = 0
    protocol         = "-1"
    cidr_blocks      = ["0.0.0.0/0"]
    ipv6_cidr_blocks = ["::/0"]
  }
  tags = { Name = "${local.name}-server" }
}

# ---------------------------------------------------------------- storage

resource "aws_ecr_repository" "server" {
  name                 = "${local.name}-server"
  image_tag_mutability = "MUTABLE"
  force_delete         = true
  image_scanning_configuration {
    scan_on_push = true
  }
}

resource "aws_ecr_lifecycle_policy" "server" {
  repository = aws_ecr_repository.server.name
  policy     = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep the last 15 images"
      selection    = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 15 }
      action       = { type = "expire" }
    }]
  })
}

# One bucket: deploy bundles, SSM command logs and database backups.
resource "aws_s3_bucket" "ops" {
  bucket        = local.bucket_name
  force_destroy = true
}

resource "aws_s3_bucket_public_access_block" "ops" {
  bucket                  = aws_s3_bucket.ops.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "ops" {
  bucket = aws_s3_bucket.ops.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "ops" {
  bucket = aws_s3_bucket.ops.id
  rule {
    id     = "backups"
    status = "Enabled"
    filter {
      prefix = "backups/"
    }
    expiration {
      days = var.backup_retention_days
    }
  }
  rule {
    id     = "bundles-and-logs"
    status = "Enabled"
    filter {
      prefix = "deploy/"
    }
    expiration {
      days = 30
    }
  }
}

# ---------------------------------------------------------------- secrets
# Generated once and kept in SSM Parameter Store (encrypted). Ansible reads
# them on the server; they never appear in git or in the pipeline log.

resource "random_password" "secret" {
  for_each = toset(["db_password", "session_key", "refresh_key", "http_key", "cluster_secret"])
  length   = 40
  special  = false
}

resource "random_password" "server_key" {
  length  = 24
  special = false
}

resource "aws_ssm_parameter" "secret" {
  for_each = random_password.secret
  name     = "${local.param_path}/${each.key}"
  type     = "SecureString"
  value    = each.value.result
}

# The socket server key is built into the app, so it is not a secret; it is
# stored like the others and printed by the pipeline for the Unity client.
resource "aws_ssm_parameter" "server_key" {
  name  = "${local.param_path}/server_key"
  type  = "SecureString"
  value = "tango-${random_password.server_key.result}"
}

# ---------------------------------------------------------------- IAM

data "aws_iam_policy_document" "assume_ec2" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "server" {
  name               = "${local.name}-server"
  assume_role_policy = data.aws_iam_policy_document.assume_ec2.json
}

resource "aws_iam_role_policy_attachment" "ssm_core" {
  role       = aws_iam_role.server.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "server" {
  statement {
    sid       = "EcrAuth"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }
  statement {
    sid       = "EcrPull"
    actions   = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability"]
    resources = [aws_ecr_repository.server.arn]
  }
  statement {
    sid       = "ReadSecrets"
    actions   = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
    resources = ["arn:aws:ssm:${var.region}:${data.aws_caller_identity.me.account_id}:parameter${local.param_path}*"]
  }
  statement {
    sid       = "DecryptSecrets"
    actions   = ["kms:Decrypt"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["ssm.${var.region}.amazonaws.com"]
    }
  }
  statement {
    sid       = "OpsBucket"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:ListBucket"]
    resources = [aws_s3_bucket.ops.arn, "${aws_s3_bucket.ops.arn}/*"]
  }
}

resource "aws_iam_role_policy" "server" {
  name   = "${local.name}-server"
  role   = aws_iam_role.server.id
  policy = data.aws_iam_policy_document.server.json
}

resource "aws_iam_instance_profile" "server" {
  name = "${local.name}-server"
  role = aws_iam_role.server.name
}

# ---------------------------------------------------------------- server

resource "aws_instance" "server" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.instance_type
  subnet_id              = aws_subnet.public.id
  vpc_security_group_ids = [aws_security_group.server.id]
  iam_instance_profile   = aws_iam_instance_profile.server.name
  user_data              = file("${path.module}/user_data.sh")

  # Burst credits only; never pay for sustained CPU above baseline.
  credit_specification {
    cpu_credits = "standard"
  }

  metadata_options {
    http_tokens                 = "required" # IMDSv2 only
    http_put_response_hop_limit = 2
  }

  root_block_device {
    volume_type           = "gp3"
    volume_size           = var.root_volume_gb
    encrypted             = true
    delete_on_termination = true
  }

  lifecycle {
    # A newer Ubuntu image must not replace a running server by surprise.
    ignore_changes = [ami, user_data]
  }

  tags = { Name = "${local.name}-server" }
}

resource "aws_eip" "server" {
  domain = "vpc"
  tags   = { Name = "${local.name}-server" }
}

resource "aws_eip_association" "server" {
  instance_id   = aws_instance.server.id
  allocation_id = aws_eip.server.id
}

resource "aws_route53_record" "server" {
  count   = var.domain_name != "" && var.route53_zone_id != "" ? 1 : 0
  zone_id = var.route53_zone_id
  name    = var.domain_name
  type    = "A"
  ttl     = 300
  records = [aws_eip.server.public_ip]
}
