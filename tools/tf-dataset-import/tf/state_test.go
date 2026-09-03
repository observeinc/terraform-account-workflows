package tf

import (
	"os"
	"testing"
)

const fakeState = "../testdata/fake-repo-state.json"

func TestNormalizeOID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"o:::dataset:99015371/2026-01-05T17:07:19Z", "o:::dataset:99015371"},
		{"o:::dataset:99007951", "o:::dataset:99007951"},
		{"o:::workspace:99000009", "o:::workspace:99000009"},
		{"", ""},
	} {
		if got := NormalizeOID(tc.in); got != tc.want {
			t.Errorf("NormalizeOID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsOID(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"o:::dataset:99007951", true},
		{"o:::storageintegration:99085251", true},
		{"o:99000001:99000002:dataset:99007951", true},
		{"network_logs", false},
		{"o:::dataset", false},
		{"", false},
	} {
		if got := IsOID(tc.in); got != tc.want {
			t.Errorf("IsOID(%q) = %t, want %t", tc.in, got, tc.want)
		}
	}
}

func TestBuildIndexSeparatesManagedFromData(t *testing.T) {
	s, err := LoadState(fakeState)
	if err != nil {
		t.Fatal(err)
	}
	idx := BuildIndex(s, "module.example")

	if e, ok := idx.ManagedInModule["o:::dataset:99001000"]; !ok || e.Address != "observe_dataset.existing_peer" {
		t.Errorf("managed peer indexed as %+v (ok=%t)", e, ok)
	}
	if e, ok := idx.DataInModule["o:::dataset:99002000"]; !ok || e.Address != "data.observe_dataset.dashboard_fake__dataset_only_via_data_source" {
		t.Errorf("in-module data source indexed as %+v (ok=%t)", e, ok)
	}
	// A module-relative address omits the `module.<x>.` prefix, since references inside the module
	// do not carry it.
	for oid, e := range idx.ManagedInModule {
		if len(e.Address) > 7 && e.Address[:7] == "module." {
			t.Errorf("%s indexed with a module prefix: %s", oid, e.Address)
		}
	}

	// A datastream exposes both its own oid and the dataset it writes into.
	b, ok := idx.RootByAddress["data.observe_datastream.applogs"]
	if !ok {
		t.Fatal("root datastream not indexed")
	}
	if b.OID != "o:::datastream:99000100" || b.DatasetOID != "o:::dataset:99000101" {
		t.Errorf("datastream binding = %+v", b)
	}

	if idx.WorkspaceOID != "o:::workspace:99000001" {
		t.Errorf("workspace = %q", idx.WorkspaceOID)
	}
	if idx.NamesToOID["Fake/Existing Peer"] != "o:::dataset:99001000" {
		t.Errorf("name index = %v", idx.NamesToOID)
	}
}

// TestTransitiveDependents covers the blast radius: any config change recomputes a dataset's oid,
// which makes each dependent's inputs unknown at plan time and cascades onward.
func TestTransitiveDependents(t *testing.T) {
	s, err := LoadState(fakeState)
	if err != nil {
		t.Fatal(err)
	}
	g := BuildGraph(s)

	got := g.TransitiveDependents([]string{"o:::dataset:99010001"})
	want := map[string]bool{
		"o:::dataset:99001500": true, // direct dependent
		"o:::dataset:99001600": true, // two hops out
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, oid := range got {
		if !want[oid] {
			t.Errorf("unexpected dependent %s", oid)
		}
	}

	t.Run("seeds are excluded", func(t *testing.T) {
		for _, oid := range g.TransitiveDependents([]string{"o:::dataset:99001500"}) {
			if oid == "o:::dataset:99001500" {
				t.Error("a seed must not appear in its own dependents")
			}
		}
	})

	t.Run("suffix is normalized", func(t *testing.T) {
		suffixed := g.TransitiveDependents([]string{"o:::dataset:99010001/2025-06-01T00:00:00Z"})
		if len(suffixed) != len(got) {
			t.Errorf("suffixed seed gave %v, bare gave %v", suffixed, got)
		}
	})

	t.Run("no dependents", func(t *testing.T) {
		if n := len(g.TransitiveDependents([]string{"o:::dataset:99001600"})); n != 0 {
			t.Errorf("want 0 dependents, got %d", n)
		}
	})
}

func TestLoadRejectsWrongVersion(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	if err := os.WriteFile(path, []byte(`{"version": 3, "resources": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Error("want an error for a non-v4 state file")
	}
}

// TestIndexIgnoresAReferenceToAnotherDatasetsOID is the regression test for a resource claiming an oid
// it only points at.
//
// A correlation tag's `dataset` names the dataset being tagged; a reference table's names the dataset
// holding its rows. Indexing either as that resource's own identity let it take the dataset's key, and
// state is ordered alphabetically by type, so `observe_correlation_tag` always came first and won —
// deterministically, for a deterministic subset of managed datasets. Build skips a non-dataset
// entry, so the dataset then had no peer reference at all.
//
// The fake state places both ahead of the datasets they reference, reproducing that ordering.
func TestIndexIgnoresAReferenceToAnotherDatasetsOID(t *testing.T) {
	s, err := LoadState(fakeState)
	if err != nil {
		t.Fatal(err)
	}
	idx := BuildIndex(s, "module.example")

	for _, tc := range []struct{ oid, wantAddress, referencedBy string }{
		{"o:::dataset:99001000", "observe_dataset.existing_peer", "observe_correlation_tag.peer_ip_address"},
		{"o:::dataset:99001500", "observe_dataset.downstream_of_peer", "observe_reference_table.peer_lookup"},
	} {
		e, ok := idx.ManagedInModule[tc.oid]
		if !ok {
			t.Errorf("%s is not indexed at all", tc.oid)
			continue
		}
		if e.Address != tc.wantAddress {
			t.Errorf("%s resolved to %s, want %s — %s claimed a key it only references",
				tc.oid, e.Address, tc.wantAddress, tc.referencedBy)
		}
	}

	// The reference table keeps its own oid, which is in a namespace of its own.
	if e, ok := idx.ManagedInModule["o:::referencetable:99001400"]; !ok || e.Address != "observe_reference_table.peer_lookup" {
		t.Errorf("a reference table should still be indexed under its own oid, got %+v", e)
	}
}

// TestDatastreamKeepsItsDatasetOID guards the case the dataset-attribute indexing exists for: a
// datastream's `dataset` is the dataset it writes into, which is its own identity as far as a module
// variable is concerned, so narrowing the rule must not drop it.
func TestDatastreamKeepsItsDatasetOID(t *testing.T) {
	if got := identityOIDs("observe_datastream", "o:::datastream:1", "o:::dataset:2"); len(got) != 2 {
		t.Errorf("a datastream should be indexed under both oids, got %v", got)
	}
	for _, typ := range []string{"observe_correlation_tag", "observe_reference_table", "observe_dataset"} {
		got := identityOIDs(typ, "o:::x:1", "o:::dataset:2")
		if len(got) != 1 || got[0] != "o:::x:1" {
			t.Errorf("%s should be indexed under its own oid only, got %v", typ, got)
		}
	}
}

func TestGroupNamesAreIndexedFromState(t *testing.T) {
	s, err := LoadState(fakeState)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	idx := BuildIndex(s, "module.example")

	if len(idx.GroupNames) == 0 {
		t.Fatal("expected GroupNames to be populated from observe_rbac_group resources")
	}
	if got := idx.GroupNames["5001"]; got != "TEST-VIEWER" {
		t.Errorf("GroupNames[5001] = %q, want TEST-VIEWER", got)
	}
	if got := idx.GroupNames["5002"]; got != "TEST-WRITER" {
		t.Errorf("GroupNames[5002] = %q, want TEST-WRITER", got)
	}
}
