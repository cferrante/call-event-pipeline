variable "region" {
  description = "AWS region; the bootstrap's REGION."
  type        = string
  default     = "us-east-1"
}

variable "sut_type" {
  description = "Instance type of the system under test."
  type        = string
  default     = "c7g.xlarge"
}

variable "loadgen_type" {
  description = "Instance type of the load generator."
  type        = string
  default     = "c7g.large"
}

variable "disk_gb" {
  description = "Root volume size of each instance, in GB (gp3)."
  type        = number
  default     = 20
}

variable "ingest_port" {
  description = "Port the ingest service listens on; the only port the load generator may reach."
  type        = number
  default     = 8081
}

variable "expiry_minutes" {
  description = "Minutes after first boot when each instance powers off and so terminates itself."
  type        = number
  default     = 180

  validation {
    condition     = var.expiry_minutes >= 5 && var.expiry_minutes <= 180
    error_message = "expiry_minutes must be between 5 and 180: the 3-hour guard is the cost limit."
  }
}
