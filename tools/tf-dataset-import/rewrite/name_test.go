package rewrite

import (
	"fmt"
	"strings"
	"testing"

	"tfdatasetimport/observe"
	"tfdatasetimport/tf"
)

// def builds a generated definition with a given live dataset name. importName is left empty so the
// tool derives a resource name, which is what puts the derived name and the managing resource's name
// in disagreement.
func def(datasetName string) *observe.TerraformDefinition {
	return &observe.TerraformDefinition{
		Resource: fmt.Sprintf(`resource "observe_dataset" "generated" {
  workspace = "o:::workspace:99000001"
  name      = %q
  inputs = {
    "applogs" = "o:::dataset:99000101"
  }
  stage {
    pipeline = <<-PIPE
      filter true
    PIPE
  }
}`, datasetName),
	}
}

// TestAlreadyManagedIsFoundByIdentityNotByName is the regression test for the check that decides
// whether a dataset is a create or an update.
//
// The old check looked up the resource name it *wanted* and asked whether the resource sitting there
// described the same dataset. That only answers correctly when the existing resource happens to carry
// the name this tool would derive. The reference repo hand-names 308 of its 661 datasets, so the usual
// case is a miss — and a miss here is not benign: the run reports a collision and suggests a free name,
// and taking that suggestion adds a second resource importing the same dataset id to a second address.
func TestAlreadyManagedIsFoundByIdentityNotByName(t *testing.T) {
	repo, err := tf.LoadRepo(fakeRepo, moduleDir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := tf.LoadState(fakeState)
	if err != nil {
		t.Fatal(err)
	}
	index := tf.BuildIndex(state, moduleAddress)

	t.Run("by dataset name, when the derived name is taken by something else", func(t *testing.T) {
		// `Fake/Hand Named Thing` is managed as observe_dataset.fake_hand_named; the derived name
		// `hand_named_thing` belongs to an unrelated dataset. This is the exact shape that produced a
		// `_2` suggestion.
		defs := map[string]*observe.TerraformDefinition{"99009999": def("Fake/Hand Named Thing")}

		names, alreadyManaged, collisions := AssignNames(defs, repo, index, nil)

		if len(collisions) != 0 {
			t.Errorf("an already-managed dataset must not be reported as a collision: %v", collisions)
		}
		for _, c := range collisions {
			if strings.HasSuffix(c.Suggestion, "_2") {
				t.Errorf("suggested %q, which would be a second resource for one dataset", c.Suggestion)
			}
		}
		// An update keeps the existing resource's name — it is not assigned a fresh one.
		if names["99009999"] != "fake_hand_named" {
			t.Errorf("want it kept under fake_hand_named, got %q", names["99009999"])
		}
		if where, ok := alreadyManaged["99009999"]; !ok || !strings.Contains(where, "fake_hand_named") {
			t.Errorf("want it recorded as already managed by fake_hand_named, got %v", alreadyManaged)
		}
	})

	t.Run("by dataset id in state, whatever the name derives to", func(t *testing.T) {
		// 99001000 is module.example.existing_peer in state. The live name here derives a free
		// identifier, so nothing about the name would flag it — only the id does.
		defs := map[string]*observe.TerraformDefinition{"99001000": def("Fake/Totally Fresh Name")}

		names, alreadyManaged, collisions := AssignNames(defs, repo, index, nil)

		if names["99001000"] != "existing_peer" {
			t.Errorf("want it kept under existing_peer, got %q", names["99001000"])
		}
		if len(collisions) != 0 {
			t.Errorf("unexpected collisions: %v", collisions)
		}
		if where, ok := alreadyManaged["99001000"]; !ok || !strings.Contains(where, "existing_peer") {
			t.Errorf("want it recorded as already managed by existing_peer, got %v", alreadyManaged)
		}
	})

	t.Run("a genuinely new dataset is still assigned", func(t *testing.T) {
		defs := map[string]*observe.TerraformDefinition{"99009998": def("Fake/Nothing Manages This")}

		names, alreadyManaged, collisions := AssignNames(defs, repo, index, nil)

		if names["99009998"] != "nothing_manages_this" {
			t.Errorf("want the derived name, got %q", names["99009998"])
		}
		if len(alreadyManaged) != 0 || len(collisions) != 0 {
			t.Errorf("want a clean assignment, got alreadyManaged=%v collisions=%v", alreadyManaged, collisions)
		}
	})
}

// TestCollisionMessageDoesNotImplyAnHCLConflict covers the wording for a name taken by a different
// resource type. Terraform namespaces resource names per type, so it would accept
// observe_dataset.hand_named_thing beside observe_link.hand_named_thing; the actual obstacle is that
// this tool writes one <name>.tf per resource. Saying only "taken" would send the reader looking for a
// conflict terraform does not have.
func TestCollisionMessageDoesNotImplyAnHCLConflict(t *testing.T) {
	repo, err := tf.LoadRepo(fakeRepo, moduleDir)
	if err != nil {
		t.Fatal(err)
	}

	// dashboard_data_source.tf declares this as a data source, not a managed resource, so terraform
	// would accept a resource of the same type and name beside it.
	const name = "dashboard_fake__dataset_only_via_data_source"
	existing, ok := repo.Existing[name]
	if !ok {
		t.Skip("fake repo no longer declares the non-resource this test relies on")
	}
	if existing.IsResource {
		t.Skipf("%s is now a managed resource; pick another non-resource name", existing.Address)
	}

	defs := map[string]*observe.TerraformDefinition{
		"99009997": {Resource: def("Fake/Whatever").Resource, ImportName: name},
	}
	_, _, collisions := AssignNames(defs, repo, nil, nil)
	if len(collisions) != 1 {
		t.Fatalf("want 1 collision, got %v", collisions)
	}
	got := collisions[0].String()
	for _, want := range []string{"terraform would permit", ".tf, which already exists"} {
		if !strings.Contains(got, want) {
			t.Errorf("message should explain the real obstacle, missing %q in:\n%s", want, got)
		}
	}
}
