variable "endpoint" {
  description = "Public HTTPS URL of the changefeed server, e.g. https://changefeed.example.com"
  type        = string

  validation {
    condition     = startswith(var.endpoint, "https://")
    error_message = "endpoint must use https:// (EventBridge API destinations require it)."
  }
}

variable "token" {
  description = "changefeed token with scopes [write] and sources [aws]. Stored by EventBridge in Secrets Manager."
  type        = string
  sensitive   = true
}

variable "name" {
  description = "Name prefix for the created resources"
  type        = string
  default     = "changefeed"
}

variable "services" {
  description = "AWS service prefixes to forward (the part after aws. in the event source)"
  type        = list(string)
  default     = ["rds", "iam", "ec2", "eks", "elasticloadbalancing", "autoscaling", "lambda", "s3", "elasticache", "route53", "kms"]
}

variable "event_bus_name" {
  description = "Event bus that receives CloudTrail events (default bus, or an org-wide bus)"
  type        = string
  default     = "default"
}

variable "rate_limit_per_second" {
  description = "Maximum deliveries per second to changefeed"
  type        = number
  default     = 20
}

variable "alarm_sns_topic_arn" {
  description = "SNS topic for an alarm when events land in the dead-letter queue. Empty disables the alarm."
  type        = string
  default     = ""
}

variable "tags" {
  description = "Extra tags for created resources"
  type        = map(string)
  default     = {}
}
