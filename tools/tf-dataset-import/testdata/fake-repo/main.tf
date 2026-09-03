locals {
  in_west = data.observe_workspace.default.id == "99000215"
}

data "observe_workspace" "default" {
  name = "Default"
}

data "observe_datastream" "applogs" {
  workspace = data.observe_workspace.default.oid
  name      = "AppLogs"
}

data "observe_datastream" "networklogs" {
  workspace = data.observe_workspace.default.oid
  name      = "NetworkLogs"
}

data "observe_dataset" "usage_raw_usage_events" {
  workspace = data.observe_workspace.default.oid
  name      = "usage/Raw Usage Events"
}

module "piedpiper" {
  source     = "./modules/piedpiper"
  workspace  = data.observe_workspace.default
  datastream = data.observe_datastream.applogs
}

module "example" {
  source    = "./modules/example"
  workspace = data.observe_workspace.default

  applogs_datastream     = data.observe_datastream.applogs
  networklogs_datastream = data.observe_datastream.networklogs
  usage_raw_usage_events = data.observe_dataset.usage_raw_usage_events
  piedpiper              = module.piedpiper

  freshness_overrides = {
    "existing_peer" = "1h",
  }

  freshness_default = "5m"
}
