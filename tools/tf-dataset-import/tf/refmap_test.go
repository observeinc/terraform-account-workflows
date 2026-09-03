package tf

import (
	"testing"
)

func load(t *testing.T, self map[string]string) *Map {
	t.Helper()
	repo, err := LoadRepo("../testdata/fake-repo", "modules/example")
	if err != nil {
		t.Fatal(err)
	}
	state, err := LoadState("../testdata/fake-repo-state.json")
	if err != nil {
		t.Fatal(err)
	}
	if self == nil {
		self = map[string]string{}
	}
	return (&Builder{Index: BuildIndex(state, "module.example"), Repo: repo, SelfOIDs: self}).Build()
}

func TestResolveByTier(t *testing.T) {
	m := load(t, map[string]string{"o:::dataset:99010001": "new_one"})

	for _, tc := range []struct {
		name     string
		oid      string
		wantRef  string
		wantTier Tier
	}{
		{"managed peer", "o:::dataset:99001000", "observe_dataset.existing_peer.oid", TierPeer},
		{"dataset being imported", "o:::dataset:99010001", "observe_dataset.new_one.oid", TierSelf},
		{"datastream module variable", "o:::dataset:99000101", "var.applogs_datastream.dataset", TierModuleVar},
		{"dataset module variable", "o:::dataset:99000300", "var.usage_raw_usage_events.oid", TierModuleVar},
		{"workspace", "o:::workspace:99000001", "var.workspace.oid", TierWorkspace},
		{"in-module data source", "o:::dataset:99002000", "data.observe_dataset.dashboard_fake__dataset_only_via_data_source.oid", TierModuleData},
		{"storage integration", "o:::storageintegration:99003000", "data.observe_oid.storage_integration_fake.oid", TierModuleOID},
		{"unknown", "o:::dataset:99999999", "", TierUnresolved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := m.Resolve(tc.oid)
			if got.Reference != tc.wantRef {
				t.Errorf("reference = %q, want %q", got.Reference, tc.wantRef)
			}
			if got.Tier != tc.wantTier {
				t.Errorf("tier = %v, want %v", got.Tier, tc.wantTier)
			}
		})
	}
}

// TestResolveIgnoresVersionSuffix covers the mismatch the tool exists to absorb: a peer's `.oid` in
// state carries a version suffix while generated `inputs` are bare, and both must resolve.
func TestResolveIgnoresVersionSuffix(t *testing.T) {
	m := load(t, nil)
	bare := m.Resolve("o:::dataset:99001000")
	suffixed := m.Resolve("o:::dataset:99001000/2025-06-01T00:00:00Z")
	if bare.Reference != suffixed.Reference || bare.Reference == "" {
		t.Errorf("suffix changed resolution: bare=%q suffixed=%q", bare.Reference, suffixed.Reference)
	}
}

// TestManagedPeerBeatsDataSource matters because the same dataset often appears both ways; the
// data-source names are scoped to a dashboard and read poorly as a dataset input.
func TestManagedPeerBeatsDataSource(t *testing.T) {
	m := load(t, nil)
	if got := m.Resolve("o:::dataset:99001000"); got.Tier != TierPeer {
		t.Errorf("tier = %v, want TierPeer", got.Tier)
	}
}

func TestNeedsReview(t *testing.T) {
	for tier, want := range map[Tier]bool{
		TierPeer:       false,
		TierSelf:       false,
		TierModuleVar:  false,
		TierWorkspace:  false,
		TierModuleOID:  false,
		TierModuleData: true,
		TierUnresolved: true,
	} {
		if got := tier.NeedsReview(); got != want {
			t.Errorf("%v.NeedsReview() = %t, want %t", tier, got, want)
		}
	}
}

// TestDataSourceBindingsIn covers what a module variable is bound to, and — the part that matters —
// which accessor is therefore correct on it. `var.x = data.d.a` binds the object, so `var.x.dataset`
// reads its dataset oid; `var.x = data.d.a.dataset` binds a bare string and the reference is `var.x`.
func TestDataSourceBindingsIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		expr string
		want []dataSourceBinding
	}{
		{
			"whole object",
			"data.observe_datastream.applogs",
			[]dataSourceBinding{{Address: "data.observe_datastream.applogs"}},
		},
		{
			// Bound to the dataset oid itself, so the variable takes no accessor at all. Truncating
			// this to the bare address made the tool emit var.x.dataset, which fails at plan.
			"attribute suffix is recorded, not discarded",
			"data.observe_datastream.applogs.dataset",
			[]dataSourceBinding{{Address: "data.observe_datastream.applogs", Attribute: "dataset"}},
		},
		{
			// A count-gated binding. Missing this left a real input unresolved.
			"count-gated conditional",
			"local.in_west ? null : data.observe_dataset.bluecoatcim_source[0]",
			[]dataSourceBinding{{Address: "data.observe_dataset.bluecoatcim_source"}},
		},
		{
			"bare interpolation",
			`"${data.observe_datastream.applogs.dataset}"`,
			[]dataSourceBinding{{Address: "data.observe_datastream.applogs", Attribute: "dataset"}},
		},
		{
			// jsondecode changes the variable's type, so the wrapped data source is not what the
			// variable is: emitting var.aws.oid for it would be wrong.
			"function call is not a binding",
			"jsondecode(data.observe_app.aws.outputs)",
			nil,
		},
		{
			// Past an index the traversal is into a collection element, not the object.
			"indexed attribute is refused",
			`data.observe_app.aws.outputs["ds"]`,
			nil,
		},
		{"traversal too deep", "data.observe_app.aws.outputs.nested", nil},
		{"module reference", "module.piedpiper", nil},
		{
			"ambiguous conditional",
			"local.x ? data.observe_dataset.a : data.observe_dataset.b",
			[]dataSourceBinding{{Address: "data.observe_dataset.a"}, {Address: "data.observe_dataset.b"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dataSourceBindingsIn(tc.expr)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}
