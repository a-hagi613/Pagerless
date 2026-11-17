output "vpc_id" {
  description = "The VPC that holds the cluster's public subnets."
  value       = module.network.vpc_id
}

output "vpc_cidr" {
  description = "CIDR block of the VPC, used by cluster network policy descriptions."
  value       = module.network.vpc_cidr
}

output "public_subnet_ids" {
  description = "Public subnets, in Availability Zone order."
  value       = module.network.public_subnet_ids
}

output "availability_zones" {
  description = "Availability Zones the public subnets occupy."
  value       = module.network.availability_zones
}

output "node_security_group_id" {
  description = "Security group for cluster nodes. It has no inbound rule; the cluster security group carries the control plane traffic."
  value       = module.network.node_security_group_id
}
