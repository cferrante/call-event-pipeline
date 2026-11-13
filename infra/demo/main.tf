# The on-demand demo environment (design section 13): a system under test and a load
# generator in the default VPC, reachable only through AWS Systems Manager.

locals {
  name = "call-event-pipeline-demo"
  tags = { project = "call-event-pipeline" }
}

# --- Network: default VPC, one availability zone that offers both instance types ------------
# Both instances share an AZ, because traffic between AZs is charged per GB in each direction.

data "aws_vpc" "default" {
  default = true
}

data "aws_ec2_instance_type_offerings" "both" {
  location_type = "availability-zone"
  filter {
    name   = "instance-type"
    values = [var.sut_type, var.loadgen_type]
  }
}

locals {
  offered = data.aws_ec2_instance_type_offerings.both
  # AZs listed once per type they offer; keep those that offer both.
  azs = sort([for az in distinct(local.offered.locations) : az if length([for l in local.offered.locations : l if l == az]) == 2])
}

data "aws_subnet" "chosen" {
  vpc_id            = data.aws_vpc.default.id
  availability_zone = local.azs[0]
  default_for_az    = true
}

# Security groups: no inbound rules at all, except ingest's port from the load generator's
# group. Outbound is open: Systems Manager, S3 and package mirrors are reached over HTTPS.
resource "aws_security_group" "sut" {
  name        = "${local.name}-sut"
  description = "System under test: no inbound except ingest from the load generator"
  vpc_id      = data.aws_vpc.default.id
}

resource "aws_security_group" "loadgen" {
  name        = "${local.name}-loadgen"
  description = "Load generator: no inbound"
  vpc_id      = data.aws_vpc.default.id
}

resource "aws_vpc_security_group_ingress_rule" "ingest_from_loadgen" {
  security_group_id            = aws_security_group.sut.id
  description                  = "Webhooks from the load generator"
  referenced_security_group_id = aws_security_group.loadgen.id
  ip_protocol                  = "tcp"
  from_port                    = var.ingest_port
  to_port                      = var.ingest_port
}

resource "aws_vpc_security_group_egress_rule" "all_out" {
  for_each          = { sut = aws_security_group.sut.id, loadgen = aws_security_group.loadgen.id }
  security_group_id = each.value
  description       = "Outbound to AWS APIs and package mirrors"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

# --- Artifacts: binaries and units are shipped through this bucket, never a git clone -------

resource "aws_s3_bucket" "artifacts" {
  bucket_prefix = "${local.name}-"
  force_destroy = true # demo-down deletes it with its contents
}

resource "aws_s3_bucket_public_access_block" "artifacts" {
  bucket                  = aws_s3_bucket.artifacts.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# --- Instance role: Systems Manager, the artifact bucket and the demo's secrets -------------

data "aws_caller_identity" "me" {}

resource "aws_iam_role" "instance" {
  name = local.name
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy" "artifacts_and_secrets" {
  name = "artifacts-and-secrets"
  role = aws_iam_role.instance.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = "${aws_s3_bucket.artifacts.arn}/*"
      },
      {
        # SecureStrings use the AWS-managed key, whose policy already lets the account's
        # principals decrypt through SSM, so no KMS statement is needed.
        Effect = "Allow"
        Action = ["ssm:GetParametersByPath"]
        Resource = [
          "arn:aws:ssm:${var.region}:${data.aws_caller_identity.me.account_id}:parameter/call-event-pipeline/demo",
          "arn:aws:ssm:${var.region}:${data.aws_caller_identity.me.account_id}:parameter/call-event-pipeline/demo/*",
        ]
      },
    ]
  })
}

resource "aws_iam_instance_profile" "instance" {
  name = local.name
  role = aws_iam_role.instance.name
}

# --- Instances ------------------------------------------------------------------------------

data "aws_ssm_parameter" "ubuntu" {
  # Canonical's current Ubuntu 26.04 LTS image for Graviton, matching the development machine.
  name = "/aws/service/canonical/ubuntu/server/26.04/stable/current/arm64/hvm/ebs-gp3/ami-id"
}

locals {
  instances = {
    sut     = { type = var.sut_type, sg = aws_security_group.sut.id }
    loadgen = { type = var.loadgen_type, sg = aws_security_group.loadgen.id }
  }
}

resource "aws_instance" "this" {
  for_each = local.instances

  ami                    = data.aws_ssm_parameter.ubuntu.insecure_value
  instance_type          = each.value.type
  subnet_id              = data.aws_subnet.chosen.id
  vpc_security_group_ids = [each.value.sg]
  iam_instance_profile   = aws_iam_instance_profile.instance.name
  # A public address lets the SSM agent reach Systems Manager without paid VPC endpoints;
  # the security groups still allow nothing in.
  associate_public_ip_address = true

  # The cost guard (design section 13): user data sets a timer at every boot that powers the
  # instance off when its time is up, and powering off terminates it. plancheck refuses a
  # plan without either.
  instance_initiated_shutdown_behavior = "terminate"
  user_data = templatefile("${path.module}/cloud-init.yaml.tftpl", {
    role           = each.key
    region         = var.region
    expiry_seconds = var.expiry_minutes * 60
  })
  user_data_replace_on_change = true

  metadata_options {
    http_tokens = "required" # IMDSv2 only
  }

  root_block_device {
    volume_type           = "gp3"
    volume_size           = var.disk_gb
    delete_on_termination = true
  }
  volume_tags = merge(local.tags, { Name = "${local.name}-${each.key}" })

  tags = { Name = "${local.name}-${each.key}", role = each.key }
}
