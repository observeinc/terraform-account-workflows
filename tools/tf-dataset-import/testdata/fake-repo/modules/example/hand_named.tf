// A dataset the module manages under a hand-chosen name, alongside an unrelated resource occupying the
// name this tool would derive for it. This is the shape that made the name-keyed already-adopted check
// not merely miss the dataset but recommend adding a second resource for it: the reference repo
// hand-names most of its datasets, so `Fake/Hand Named Thing` is managed as `fake_hand_named` while the
// derived name is `hand_named_thing`.
resource "observe_dataset" "fake_hand_named" {
  workspace = var.workspace.oid
  name      = format(var.name_format, "Fake/Hand Named Thing")
  freshness = lookup(local.freshness, "fake_hand_named", var.freshness_default)

  inputs = {
    "applogs" = var.applogs_datastream.dataset
  }

  stage {
    pipeline = <<-EOT
      filter true
    EOT
  }
}

resource "observe_dataset" "hand_named_thing" {
  workspace = var.workspace.oid
  name      = format(var.name_format, "Fake/Unrelated Occupant")
  freshness = lookup(local.freshness, "hand_named_thing", var.freshness_default)

  inputs = {
    "applogs" = var.applogs_datastream.dataset
  }

  stage {
    pipeline = <<-EOT
      filter true
    EOT
  }
}
