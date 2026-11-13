terraform {
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
  # State is local and git-ignored (design section 13): the environment lives for one session,
  # and `tofu destroy` from this directory removes everything it created.
}

provider "aws" {
  # The profile comes from AWS_PROFILE, set by lib.sh from the bootstrap's config.env.
  region = var.region

  default_tags {
    tags = local.tags
  }
}
