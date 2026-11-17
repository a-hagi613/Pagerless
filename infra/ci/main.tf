# GitHub Actions authenticates with a short-lived OIDC token instead of an AWS
# access key (ADR-0009). Two roles keep the two jobs apart: a read-only role for
# the pull request plan, and a teardown role for the nightly destroy.

data "tls_certificate" "github" {
  url = "https://token.actions.githubusercontent.com"
}

resource "aws_iam_openid_connect_provider" "github" {
  url = "https://token.actions.githubusercontent.com"

  client_id_list = ["sts.amazonaws.com"]

  # Every certificate in the chain, so a rotation of an intermediate does not
  # silently stop the workflows from authenticating.
  thumbprint_list = [for certificate in data.tls_certificate.github.certificates : certificate.sha1_fingerprint]

  tags = {
    Name = "github-actions"
  }
}

locals {
  # Repositories created after 2026-07-15 carry immutable subject claims, which
  # include the owner and repository IDs. Both roles trust that exact form, so a
  # recycled repository name cannot inherit either role.
  subject_prefix = "repo:${split("/", var.github_repository)[0]}@${var.github_repository_owner_id}/${split("/", var.github_repository)[1]}@${var.github_repository_id}"

  state_bucket_arn = "arn:aws:s3:::${var.state_bucket}"

  # Every identity the session stack creates carries this prefix.
  name_prefix = "pagerless-"
}

################################################################################
# Plan: pull requests only, read-only, no state writes
################################################################################

data "aws_iam_policy_document" "plan_trust" {
  statement {
    sid     = "PullRequestsFromThisRepository"
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["${local.subject_prefix}:pull_request"]
    }
  }
}

data "aws_iam_policy_document" "plan" {
  statement {
    sid    = "ReadStackState"
    effect = "Allow"

    actions = ["s3:ListBucket", "s3:GetObject"]

    resources = [
      local.state_bucket_arn,
      "${local.state_bucket_arn}/phase-3/*",
    ]
  }

  statement {
    sid    = "ReadEksAmiParametersForAPlan"
    effect = "Allow"

    actions = ["ssm:GetParameter", "ssm:GetParameters"]

    # The EKS node group module resolves its Amazon Linux 2023 AMI from the
    # public EKS parameter namespace; the plan reads it before anything exists.
    resources = ["arn:aws:ssm:${var.aws_region}::parameter/aws/service/eks/optimized-ami/*"]
  }

  statement {
    sid    = "ReadInfrastructureForAPlan"
    effect = "Allow"

    actions = [
      "sts:GetCallerIdentity",
      "autoscaling:Describe*",
      "cloudwatch:Describe*",
      "cloudwatch:ListTagsForResource",
      "ec2:Describe*",
      "eks:Describe*",
      "eks:List*",
      "iam:Get*",
      "iam:List*",
      "kms:Describe*",
      "kms:List*",
      "logs:Describe*",
      "logs:ListTags*",
      "sqs:GetQueueAttributes",
      "sqs:GetQueueUrl",
      "sqs:ListQueueTags",
      "tag:GetResources",
    ]

    resources = ["*"]
  }
}

resource "aws_iam_role" "plan" {
  name               = var.plan_role_name
  description        = "Lets a pull request run a read-only terraform plan of infra/."
  assume_role_policy = data.aws_iam_policy_document.plan_trust.json

  tags = {
    Name = var.plan_role_name
  }
}

resource "aws_iam_role_policy" "plan" {
  name   = "plan"
  role   = aws_iam_role.plan.id
  policy = data.aws_iam_policy_document.plan.json
}

################################################################################
# Destroy: the default branch only, the permissions a teardown needs
################################################################################

data "aws_iam_policy_document" "destroy_trust" {
  statement {
    sid     = "DefaultBranchOfThisRepository"
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["${local.subject_prefix}:ref:refs/heads/main"]
    }
  }
}

# Destroying the stack deletes the roles and policies the cluster created, which
# PowerUserAccess deliberately excludes from its IAM allowance. Those writes are
# scoped to this project's names, which every resource the stack creates uses.
data "aws_iam_policy_document" "destroy_identities" {
  statement {
    sid    = "ManageThisProjectsIdentities"
    effect = "Allow"

    actions = [
      "iam:AddRoleToInstanceProfile",
      "iam:AttachRolePolicy",
      "iam:CreateInstanceProfile",
      "iam:CreatePolicy",
      "iam:CreatePolicyVersion",
      "iam:CreateRole",
      "iam:DeleteInstanceProfile",
      "iam:DeletePolicy",
      "iam:DeletePolicyVersion",
      "iam:DeleteRole",
      "iam:DeleteRolePolicy",
      "iam:DetachRolePolicy",
      "iam:GetInstanceProfile",
      "iam:GetPolicy",
      "iam:GetPolicyVersion",
      "iam:GetRole",
      "iam:GetRolePolicy",
      "iam:ListAttachedRolePolicies",
      "iam:ListInstanceProfileTags",
      "iam:ListInstanceProfilesForRole",
      "iam:ListPolicyTags",
      "iam:ListPolicyVersions",
      "iam:ListRolePolicies",
      "iam:ListRoleTags",
      "iam:PassRole",
      "iam:RemoveRoleFromInstanceProfile",
      "iam:TagInstanceProfile",
      "iam:TagPolicy",
      "iam:TagRole",
      "iam:UntagInstanceProfile",
      "iam:UntagPolicy",
      "iam:UntagRole",
      "iam:UpdateAssumeRolePolicy",
    ]

    resources = [
      "arn:aws:iam::${var.aws_account_id}:instance-profile/${local.name_prefix}*",
      "arn:aws:iam::${var.aws_account_id}:policy/${local.name_prefix}*",
      "arn:aws:iam::${var.aws_account_id}:role/${local.name_prefix}*",
    ]
  }
}

resource "aws_iam_role" "destroy" {
  name               = var.destroy_role_name
  description        = "Lets the nightly workflow destroy any remaining Pagerless cluster."
  assume_role_policy = data.aws_iam_policy_document.destroy_trust.json

  tags = {
    Name = var.destroy_role_name
  }
}

resource "aws_iam_role_policy_attachment" "destroy_power_user" {
  role       = aws_iam_role.destroy.name
  policy_arn = "arn:aws:iam::aws:policy/PowerUserAccess"
}

resource "aws_iam_role_policy" "destroy_identities" {
  name   = "pagerless-identities"
  role   = aws_iam_role.destroy.id
  policy = data.aws_iam_policy_document.destroy_identities.json
}
