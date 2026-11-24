# Forwards CloudTrail write events from EventBridge to changefeed.
# changefeed never gets AWS credentials: AWS pushes to it with a scoped token.
#
# Requires a CloudTrail trail in the account (management events, write or all).
# EventBridge receives "AWS API Call via CloudTrail" events in the trail's region;
# for global services such as IAM, deploy this module in us-east-1 as well.

locals {
  sources = [for s in var.services : "aws.${s}"]
  tags    = merge({ "managed-by" = "terraform", "app" = "changefeed" }, var.tags)
}

resource "aws_cloudwatch_event_connection" "changefeed" {
  name               = "${var.name}-connection"
  description        = "changefeed write-only token for the aws lane"
  authorization_type = "API_KEY"

  auth_parameters {
    api_key {
      key   = "Authorization"
      value = "Bearer ${var.token}"
    }
  }
}

resource "aws_cloudwatch_event_api_destination" "changefeed" {
  name                             = var.name
  description                      = "changefeed /hooks/aws"
  invocation_endpoint              = "${trimsuffix(var.endpoint, "/")}/hooks/aws"
  http_method                      = "POST"
  invocation_rate_limit_per_second = var.rate_limit_per_second
  connection_arn                   = aws_cloudwatch_event_connection.changefeed.arn
}

resource "aws_cloudwatch_event_rule" "changes" {
  name           = var.name
  description    = "CloudTrail write events for changefeed"
  event_bus_name = var.event_bus_name
  tags           = local.tags

  event_pattern = jsonencode({
    "detail-type" = ["AWS API Call via CloudTrail"]
    source        = local.sources
    detail = {
      readOnly  = [false]
      errorCode = [{ exists = false }]
      eventName = [{ "anything-but" = { prefix = "Describe" } }]
    }
  })
}

resource "aws_cloudwatch_event_target" "changefeed" {
  rule           = aws_cloudwatch_event_rule.changes.name
  event_bus_name = var.event_bus_name
  target_id      = "changefeed"
  arn            = aws_cloudwatch_event_api_destination.changefeed.arn
  role_arn       = aws_iam_role.invoke.arn

  retry_policy {
    maximum_event_age_in_seconds = 3600
    maximum_retry_attempts       = 10
  }

  dead_letter_config {
    arn = aws_sqs_queue.dlq.arn
  }
}

# Events that could not be delivered after retries land here; alarm on it.
resource "aws_sqs_queue" "dlq" {
  name                      = "${var.name}-dlq"
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
  tags                      = local.tags
}

data "aws_iam_policy_document" "dlq" {
  statement {
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.dlq.arn]
    principals {
      type        = "Service"
      identifiers = ["events.amazonaws.com"]
    }
    condition {
      test     = "ArnEquals"
      variable = "aws:SourceArn"
      values   = [aws_cloudwatch_event_rule.changes.arn]
    }
  }
}

resource "aws_sqs_queue_policy" "dlq" {
  queue_url = aws_sqs_queue.dlq.id
  policy    = data.aws_iam_policy_document.dlq.json
}

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["events.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

data "aws_caller_identity" "current" {}

resource "aws_iam_role" "invoke" {
  name               = "${var.name}-invoke"
  assume_role_policy = data.aws_iam_policy_document.assume.json
  tags               = local.tags
}

data "aws_iam_policy_document" "invoke" {
  statement {
    actions   = ["events:InvokeApiDestination"]
    resources = [aws_cloudwatch_event_api_destination.changefeed.arn]
  }
}

resource "aws_iam_role_policy" "invoke" {
  name   = "invoke-api-destination"
  role   = aws_iam_role.invoke.id
  policy = data.aws_iam_policy_document.invoke.json
}

resource "aws_cloudwatch_metric_alarm" "dlq" {
  count               = var.alarm_sns_topic_arn == "" ? 0 : 1
  alarm_name          = "${var.name}-undelivered-events"
  alarm_description   = "CloudTrail changes could not be delivered to changefeed"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = aws_sqs_queue.dlq.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  alarm_actions       = [var.alarm_sns_topic_arn]
  tags                = local.tags
}
