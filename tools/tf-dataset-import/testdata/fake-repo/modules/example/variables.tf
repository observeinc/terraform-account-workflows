variable "workspace" {
  type        = object({ oid = string, id = string })
  description = "Workspace to apply module to."
}

variable "name_format" {
  type        = string
  description = "Format string to use for dataset names. Override to introduce a prefix or suffix."
  default     = "%s"
}

variable "freshness_overrides" {
  type        = map(string)
  description = "Freshness overrides by dataset. If absent, fall back to freshness_default"
  default     = {}
}

variable "freshness_default" {
  type        = string
  description = "Default dataset freshness. Can be overridden with freshness input"
  default     = "5m"
}

variable "applogs_datastream" {
  type        = object({ dataset = string })
  description = "AppLogs Datastream"
}

variable "networklogs_datastream" {
  type        = object({ dataset = string })
  description = "NetworkLogs Datastream"
}

variable "usage_raw_usage_events" {
  type        = object({ oid = string })
  description = "Raw usage events dataset"
}

variable "piedpiper" {
  description = "The piedpiper module"
}
