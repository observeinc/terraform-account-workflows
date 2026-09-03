locals {
  freshness = merge({
    container_logs = "1s",
  }, var.freshness_overrides)
}
