# With API authentication, a node authenticates through an access entry rather
# than through the aws-auth ConfigMap. The managed node group's IAM role needs
# one before its instance can register, and the Karpenter node role gets its own
# from the karpenter submodule.
resource "aws_eks_access_entry" "system_nodes" {
  cluster_name  = module.eks.cluster_name
  principal_arn = module.eks.eks_managed_node_groups["system"].iam_role_arn
  type          = "EC2_LINUX"
}
