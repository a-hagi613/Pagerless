variable "aws_account_id" {
  description = "The only AWS account this configuration may change."
  type        = string
  default     = "207567775100"
}

variable "aws_region" {
  description = "Region of every resource and of the Bedrock endpoint the agent calls."
  type        = string
  default     = "us-east-1"
}

variable "cluster_name" {
  description = "Name of the EKS cluster and the prefix of every resource named after it."
  type        = string
  default     = "pagerless"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}[a-z0-9]$", var.cluster_name))
    error_message = "The cluster name must be 3 to 32 lowercase alphanumeric or hyphen characters, starting with a letter."
  }
}

variable "vpc_cidr" {
  description = "CIDR block of the VPC. Each public subnet takes one /24 of it."
  type        = string
  default     = "10.0.0.0/16"

  validation {
    condition     = can(cidrhost(var.vpc_cidr, 0)) && tonumber(split("/", var.vpc_cidr)[1]) <= 16
    error_message = "The VPC CIDR must be a valid IPv4 CIDR block of /16 or larger, so that three /24 subnets fit."
  }
}

variable "availability_zone_count" {
  description = "Number of public subnets, one per Availability Zone. Karpenter spreads Spot nodes across all of them."
  type        = number
  default     = 3

  validation {
    condition     = var.availability_zone_count >= 2 && var.availability_zone_count <= 6
    error_message = "Use between two and six Availability Zones."
  }
}
