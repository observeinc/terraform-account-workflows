resource "observe_dataset" "existing_peer" {
  workspace = var.workspace.oid
  name      = format(var.name_format, "Fake/Existing Peer")
  freshness = lookup(local.freshness, "existing_peer", var.freshness_default)

  inputs = {
    "applogs" = var.applogs_datastream.dataset
  }

  stage {
    pipeline = <<-EOT
      filter true
    EOT
  }
}
