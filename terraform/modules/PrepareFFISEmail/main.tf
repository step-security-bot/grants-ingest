terraform {
  required_version = "1.3.9"
  required_providers {
    aws = "~> 4.55.0"
  }
}

locals {
  dd_tags = merge(
    {
      for item in compact(split(",", try(var.additional_environment_variables.DD_TAGS, ""))) :
      split(":", trimspace(item))[0] => try(split(":", trimspace(item))[1], "")
    },
    var.datadog_custom_tags,
    { handlername = lower(var.function_name), },
  )
}

data "aws_s3_bucket" "ffis_data" {
  bucket = var.ffis_email_delivery_bucket_name
}

data "aws_s3_bucket" "source_data" {
  bucket = var.grants_source_data_bucket_name
}

module "lambda_execution_policy" {
  source  = "cloudposse/iam-policy/aws"
  version = "0.4.0"

  iam_source_policy_documents = var.additional_lambda_execution_policy_documents
  iam_policy_statements = {
    AllowS3DownloadSourceData = {
      effect  = "Allow"
      actions = ["s3:GetObject"]
      resources = [
        "${data.aws_s3_bucket.ffis_data.arn}/ses/ffis_ingest/new/*"
      ]
    }
    AllowInspectS3PreparedData = {
      effect = "Allow"
      actions = [
        "s3:GetObject",
        "s3:ListBucket"
      ]
      resources = [
        data.aws_s3_bucket.source_data.arn,
        "${data.aws_s3_bucket.source_data.arn}/sources/*/*/*/ffis/raw.eml"
      ]
    }
    AllowS3UploadPreparedData = {
      effect  = "Allow"
      actions = ["s3:PutObject"]
      resources = [
        "${data.aws_s3_bucket.prepared_data.arn}/sources/*/*/*/ffis/raw.eml"
      ]
    }
  }
}

module "lambda_function" {
  source  = "terraform-aws-modules/lambda/aws"
  version = "4.12.1"

  function_name = "${var.namespace}-${var.function_name}"
  description   = "Moves email received from FFIS to Grant Sources S3 bucket for processing."

  role_permissions_boundary         = var.permissions_boundary_arn
  attach_cloudwatch_logs_policy     = true
  cloudwatch_logs_retention_in_days = var.log_retention_in_days
  attach_policy_json                = true
  policy_json                       = module.lambda_execution_policy.json

  handler       = "bootstrap"
  runtime       = "provided.al2"
  architectures = [var.lambda_arch]
  publish       = true
  layers        = var.lambda_layer_arns

  source_path = [{
    path = var.lambda_code_path
    commands = [
      "task build-PrepareFFISEmail",
      "cd bin/PrepareFFISEmail",
      ":zip",
    ],
  }]
  store_on_s3               = true
  s3_bucket                 = var.lambda_artifact_bucket
  s3_server_side_encryption = "AES256"

  timeout     = 300 # 5 minutes, in seconds
  memory_size = 1024
  environment_variables = merge(var.additional_environment_variables, {
    DD_TRACE_RATE_LIMIT            = "1000"
    DD_TAGS                        = join(",", sort([for k, v in local.dd_tags : "${k}:${v}"]))
    DOWNLOAD_CHUNK_LIMIT           = "20"
    GRANTS_SOURCE_DATA_BUCKET_NAME = data.aws_s3_bucket.source_data.id
    LOG_LEVEL                      = var.log_level
    MAX_CONCURRENT_UPLOADS         = "10"
    S3_USE_PATH_STYLE              = "true"
  })

  allowed_triggers = {
    S3BucketNotification = {
      principal  = "s3.amazonaws.com"
      source_arn = data.aws_s3_bucket.ffis_data.arn
    }
  }
}
