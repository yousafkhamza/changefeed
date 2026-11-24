output "rule_arn" {
  description = "EventBridge rule that selects CloudTrail write events"
  value       = aws_cloudwatch_event_rule.changes.arn
}

output "dead_letter_queue_url" {
  description = "Undelivered events land here; it should stay empty"
  value       = aws_sqs_queue.dlq.id
}
