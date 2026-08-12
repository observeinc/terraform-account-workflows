package tf

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const fakeRepo = "../testdata/fake-repo"

func TestLoadFindsModuleBlockBySource(t *testing.T) {
	repo, err := LoadRepo(fakeRepo, "modules/example")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The repo has two module blocks, so matching on `source` rather than block name is what keeps
	// example's bindings apart from piedpiper's.
	if !strings.HasSuffix(repo.RootConfigPath, "main.tf") {
		t.Errorf("unexpected root config %q", repo.RootConfigPath)
	}
	for _, want := range []string{"applogs_datastream", "networklogs_datastream", "usage_raw_usage_events", "piedpiper"} {
		if _, ok := repo.Bindings[want]; !ok {
			t.Errorf("missing binding %q; got %v", want, keys(repo.Bindings))
		}
	}
	if repo.Bindings["applogs_datastream"] != "data.observe_datastream.applogs" {
		t.Errorf("binding target = %q", repo.Bindings["applogs_datastream"])
	}
	// `datastream` belongs to the piedpiper block and must not leak into example's bindings.
	if _, leaked := repo.Bindings["datastream"]; leaked {
		t.Error("piedpiper binding leaked into the example module")
	}
	if !repo.HasFreshnessOverrides {
		t.Error("want HasFreshnessOverrides")
	}
}

func TestLoadScansExistingNamesCaseInsensitively(t *testing.T) {
	repo, err := LoadRepo(fakeRepo, "modules/example")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"existing_peer",
		"dashboard_fake__dataset_only_via_data_source", // data sources count too
	} {
		if _, ok := repo.Existing[want]; !ok {
			t.Errorf("missing existing name %q; got %v", want, keys(repo.Existing))
		}
	}
	if _, ok := repo.Existing["EXISTING_PEER"]; ok {
		t.Error("names should be keyed lowercased")
	}
}

// TestAddFreshnessOverrides covers all three outcomes for a key: a new one is added, an existing one
// with a different value is updated in place, and an existing one with the same value is left alone
// entirely — which is what makes an unchanged re-run byte-identical.
func TestAddFreshnessOverrides(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(fakeRepo, "main.tf"))
	if err != nil {
		t.Fatal(err)
	}

	out, changed, err := AddFreshnessOverrides(src, "main.tf", "modules/example", []FreshnessOverride{
		{Key: "new_one", Value: "2m"},        // absent: added
		{Key: "existing_peer", Value: "30m"}, // present as "1h": updated, not duplicated
	})
	if err != nil {
		t.Fatalf("AddFreshnessOverrides: %v", err)
	}
	got := string(out)

	if !strings.Contains(got, `"new_one" = "2m"`) {
		t.Errorf("new entry missing:\n%s", got)
	}
	if n := strings.Count(got, `"existing_peer"`); n != 1 {
		t.Errorf("an updated key should not be duplicated, found %d occurrences", n)
	}
	if !strings.Contains(got, `"existing_peer" = "30m"`) {
		t.Errorf("existing value should have been updated to the current one:\n%s", got)
	}
	if strings.Contains(got, `"existing_peer" = "1h"`) {
		t.Error("the stale value should not survive the update")
	}
	wantChanged := []string{"existing_peer: 1h -> 30m", "new_one: added (2m)"}
	sort.Strings(changed)
	if fmt.Sprint(changed) != fmt.Sprint(wantChanged) {
		t.Errorf("changed = %v, want %v", changed, wantChanged)
	}
	// The piedpiper block has no freshness_overrides; editing must not have touched it.
	if !strings.Contains(got, `source     = "./modules/piedpiper"`) {
		t.Error("piedpiper module block was disturbed")
	}

	t.Run("no entries is a no-op", func(t *testing.T) {
		same, changed, err := AddFreshnessOverrides(src, "main.tf", "modules/example", nil)
		if err != nil || string(same) != string(src) || changed != nil {
			t.Errorf("want unchanged source and no changes, err=%v changed=%v", err, changed)
		}
	})

	t.Run("an entry already present with the same value is a true no-op", func(t *testing.T) {
		same, changed, err := AddFreshnessOverrides(src, "main.tf", "modules/example", []FreshnessOverride{
			{Key: "existing_peer", Value: "1h"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if string(same) != string(src) {
			t.Error("an unchanged value must not rewrite the file")
		}
		if changed != nil {
			t.Errorf("want no changes reported, got %v", changed)
		}
	})
}

func TestAddFreshnessOverridesRejectsMissingMap(t *testing.T) {
	src := []byte(`module "example" {
  source    = "./modules/example"
  workspace = "x"
}
`)
	if _, _, err := AddFreshnessOverrides(src, "main.tf", "modules/example", []FreshnessOverride{{Key: "a", Value: "1m"}}); err == nil {
		t.Error("want an error when the module block passes no freshness_overrides map")
	}
}

// TestAddFreshnessOverridesTargetsTheRightMap covers the hazard of splicing raw bytes: the insertion
// point must come from the parsed attribute's own source range, not from searching the file for text
// that matches the map. An empty `{}` matches almost anything, and two module blocks can carry
// byte-identical maps.
func TestAddFreshnessOverridesTargetsTheRightMap(t *testing.T) {
	entry := []FreshnessOverride{{Key: "k", Value: "2m"}}

	t.Run("empty map, with an earlier decoy brace", func(t *testing.T) {
		src := []byte(`variable "unrelated" {
  default = {}
}

module "example" {
  source              = "./modules/example"
  freshness_overrides = {}
}
`)
		out, _, err := AddFreshnessOverrides(src, "main.tf", "modules/example", entry)
		if err != nil {
			t.Fatal(err)
		}
		got := string(out)
		if !strings.Contains(got, `freshness_overrides = {
    "k" = "2m",}`) {
			t.Errorf("entry did not land in freshness_overrides:\n%s", got)
		}
		if !strings.Contains(got, "default = {}") {
			t.Errorf("the unrelated variable default was modified:\n%s", got)
		}
	})

	t.Run("an earlier module block with an identical map", func(t *testing.T) {
		src := []byte(`module "piedpiper" {
  source              = "./modules/piedpiper"
  freshness_overrides = {
    "a" = "1h",
  }
}

module "example" {
  source              = "./modules/example"
  freshness_overrides = {
    "a" = "1h",
  }
}
`)
		out, _, err := AddFreshnessOverrides(src, "main.tf", "modules/example", entry)
		if err != nil {
			t.Fatal(err)
		}
		// The entry must be in the second block, so everything before example's own source line is
		// unchanged.
		got := string(out)
		exampleIdx := strings.Index(got, `"./modules/example"`)
		if at := strings.Index(got, `"k" = "2m"`); at < exampleIdx {
			t.Errorf("entry landed in the piedpiper block:\n%s", got)
		}
	})
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestDeclaresOnlyDataset covers the guard that stops os.WriteFile truncating something it should not.
// The dangerous case is a module file that declares no resource at all — the resource-name scan cannot
// see those, so nothing else would catch a dataset whose generated name is `main`.
func TestDeclaresOnlyDataset(t *testing.T) {
	for _, tc := range []struct {
		name, resourceName, src string
		want                    bool
	}{
		{
			"own earlier output", "new_one",
			`resource "observe_dataset" "new_one" { name = "x" }
resource "observe_correlation_tag" "new_one_tag" { name = "t" }`,
			true,
		},
		{"a different dataset", "new_one", `resource "observe_dataset" "other" { name = "x" }`, false},
		{"module locals, no resource", "main", `locals { freshness = {} }`, false},
		{"variables only", "variables", `variable "workspace" { type = string }`, false},
		{
			// A grants companion under the dataset's exact name is part of this tool's own output for
			// it, not "something else" — an update target regenerates both together.
			"a dataset plus its grants companion", "new_one",
			`resource "observe_dataset" "new_one" { name = "x" }
resource "observe_resource_grants" "new_one" { oid = "x" }`,
			true,
		},
		{
			"a dataset plus a grants resource under a different name", "new_one",
			`resource "observe_dataset" "new_one" { name = "x" }
resource "observe_resource_grants" "other" { oid = "x" }`,
			false,
		},
		{"a tag belonging to another dataset", "new_one",
			`resource "observe_dataset" "new_one" { name = "x" }
resource "observe_correlation_tag" "other_tag" { name = "t" }`,
			false,
		},
		{"unparseable", "new_one", `resource "observe_dataset" {{{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.resourceName+".tf")
			if err := os.WriteFile(path, []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := DeclaresOnlyDataset(path, tc.resourceName)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("DeclaresOnlyDataset = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestManagesDatasetHonoursNameFormat covers the assumption the dataset-name index rests on.
//
// A declared name is a literal inside `format(var.name_format, …)`, so comparing it against a live name
// is only valid once the format is applied. The variable exists to add a prefix or suffix, and getting
// this wrong is not a cosmetic miss: an unrecognised already-managed dataset gets a second resource.
func TestManagesDatasetHonoursNameFormat(t *testing.T) {
	for _, tc := range []struct {
		name      string
		binding   string // module block's name_format; "" means unbound
		lookup    string
		wantFound bool
		wantKnown bool
	}{
		{"unbound means the identity format", "", "Fake/Hand Named Thing", true, true},
		{"explicit identity format", `"%s"`, "Fake/Hand Named Thing", true, true},
		{"a suffix is applied before comparing", `"%s (TF)"`, "Fake/Hand Named Thing (TF)", true, true},
		{"the unformatted name no longer matches", `"%s (TF)"`, "Fake/Hand Named Thing", false, true},
		{"a prefix is applied too", `"acme %s"`, "acme Fake/Hand Named Thing", true, true},
		// An expression cannot be resolved, so the index is left empty rather than filled with names
		// that would not match. NameFormatKnown is how a caller tells this from "not managed".
		{"a non-literal leaves it unanswerable", "var.some_format", "Fake/Hand Named Thing", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, err := LoadRepo(fakeRepo, "modules/example")
			if err != nil {
				t.Fatal(err)
			}
			if tc.binding == "" {
				delete(repo.Bindings, "name_format")
			} else {
				repo.Bindings["name_format"] = tc.binding
			}
			repo.indexManagedDatasets()

			if got := repo.NameFormatKnown(); got != tc.wantKnown {
				t.Errorf("NameFormatKnown = %t, want %t", got, tc.wantKnown)
			}
			existing, found := repo.ManagesDataset(tc.lookup)
			if found != tc.wantFound {
				t.Fatalf("ManagesDataset(%q) = %t, want %t", tc.lookup, found, tc.wantFound)
			}
			if found && existing.Address != "observe_dataset.fake_hand_named" {
				t.Errorf("matched the wrong resource: %s", existing.Address)
			}
		})
	}
}

// TestManagesDatasetIgnoresDataSources keeps the index to resources that actually manage a dataset. A
// data source of the same type shares the type label, so filtering on the type alone would let one in
// and make the tool skip a dataset nothing manages.
func TestManagesDatasetIgnoresDataSources(t *testing.T) {
	repo, err := LoadRepo(fakeRepo, "modules/example")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := repo.Existing["dashboard_fake__dataset_only_via_data_source"]
	if !ok {
		t.Skip("fake repo no longer declares the data source this test relies on")
	}
	if e.IsResource {
		t.Fatalf("%s should be a data source", e.Address)
	}
	if e.Type != "observe_dataset" {
		t.Fatalf("this test needs a data source of the dataset type, got %s", e.Type)
	}
	// The `data.` prefix is what makes the address honest in an error message.
	if !strings.HasPrefix(e.Address, "data.") {
		t.Errorf("a data source address should carry the data. prefix, got %s", e.Address)
	}
	if e.DatasetName != "" {
		t.Errorf("a data source declares no managed dataset, got DatasetName %q", e.DatasetName)
	}
}

// TestDeclaresOnlyImports covers the guard on the generated imports.tf. It lands in the repo root
// among hand-maintained files, so recognising our own earlier output has to be exact: anything with a
// non-import block, a top-level attribute, or nothing at all belongs to somebody else.
func TestDeclaresOnlyImports(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      bool
	}{
		{
			"own earlier output",
			`import {
  to = module.example.observe_dataset.a
  id = "1"
}
import {
  to = module.example.observe_dataset.b
  id = "2"
}`,
			true,
		},
		{"gated form", `import {
  for_each = local.in_west ? toset(["1"]) : toset([])
  to       = module.example.observe_dataset.a
  id       = each.value
}`, true},
		{"comments only, no blocks", "# nothing here yet", false},
		{"empty", "", false},
		{"a hand-written root file", `module "example" { source = "./modules/example" }`, false},
		{"imports plus something else", `import {
  to = module.example.observe_dataset.a
  id = "1"
}
locals { x = 1 }`, false},
		{"a top-level attribute", `foo = 1`, false},
		{"unparseable", `import {{{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "imports.tf")
			if err := os.WriteFile(path, []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := DeclaresOnlyImports(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("DeclaresOnlyImports = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestGrantsDeclaredSurvivesTheNameCollapse covers why grants existence can't be read off found
// directly: a dataset and its grants companion share the exact same name, and found keeps only the
// first block it sees per name (by design, for the file-collision check). The datasets and grants
// maps are populated from every block of their respective type seen, independent of that collapsing.
func TestGrantsDeclaredSurvivesTheNameCollapse(t *testing.T) {
	dir := t.TempDir()
	src := `resource "observe_dataset" "round_trip" {
  name   = "x"
  inputs = { "a" = "o:::dataset:1" }
  stage { pipeline = "filter true" }
}
resource "observe_resource_grants" "round_trip" {
  oid = observe_dataset.round_trip.oid
}
`
	if err := os.WriteFile(filepath.Join(dir, "round_trip.tf"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	found, datasets, grants, err := scanExistingResources(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The dataset block, seen first, is what found keeps for this name.
	if e := found["round_trip"]; e.Type != "observe_dataset" {
		t.Errorf("want found to keep the dataset block, got type %q", e.Type)
	}
	if e, ok := datasets["round_trip"]; !ok || e.Type != "observe_dataset" {
		t.Errorf("want datasets to hold the dataset block regardless of scan order, got %+v, ok=%t", e, ok)
	}
	if e, ok := grants["round_trip"]; !ok || e.Type != "observe_resource_grants" {
		t.Errorf("want grants to record the grants block despite the name collision, got %+v, ok=%t", e, ok)
	}
	if _, ok := grants["nothing_declares_this"]; ok {
		t.Error("an absent name must not read as declared")
	}
}

// TestDatasetsAndGrantsAreIndependentOfScanOrder is the scenario TestGrantsDeclaredSurvivesTheName-
// Collapse's own doc comment warns about but does not exercise: the grants block declared *before*
// the dataset it belongs to, in a separate file that sorts earlier. Before datasets/grants existed,
// this would have made found — and anything built from it, like the managed-dataset index — resolve
// "round_trip" to the grants resource instead of the dataset, silently losing track of where the
// dataset itself lives.
func TestDatasetsAndGrantsAreIndependentOfScanOrder(t *testing.T) {
	dir := t.TempDir()
	// "aaa_grants.tf" sorts before "zzz_dataset.tf", so the grants block is scanned first.
	grantsSrc := `resource "observe_resource_grants" "round_trip" {
  oid = observe_dataset.round_trip.oid
}
`
	datasetSrc := `resource "observe_dataset" "round_trip" {
  name   = "x"
  inputs = { "a" = "o:::dataset:1" }
  stage { pipeline = "filter true" }
}
`
	if err := os.WriteFile(filepath.Join(dir, "aaa_grants.tf"), []byte(grantsSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zzz_dataset.tf"), []byte(datasetSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	_, datasets, grants, err := scanExistingResources(dir)
	if err != nil {
		t.Fatal(err)
	}
	ds, ok := datasets["round_trip"]
	if !ok {
		t.Fatal("want the dataset resource found despite the grants block being scanned first")
	}
	if !strings.HasSuffix(ds.File, "zzz_dataset.tf") {
		t.Errorf("dataset File = %q, want zzz_dataset.tf", ds.File)
	}
	g, ok := grants["round_trip"]
	if !ok {
		t.Fatal("want the grants resource found")
	}
	if !strings.HasSuffix(g.File, "aaa_grants.tf") {
		t.Errorf("grants File = %q, want aaa_grants.tf", g.File)
	}
}

// TestExistingResourceRangeCoversExactlyItsOwnBlock covers the byte range an update relies on to
// splice a resource's fresh content into an existing file without disturbing its neighbors: it must
// start at the resource's own `resource` keyword and end at its own closing brace, not spill into
// whatever comes before or after it in the file.
func TestExistingResourceRangeCoversExactlyItsOwnBlock(t *testing.T) {
	dir := t.TempDir()
	src := `resource "observe_dataset" "before" {
  name   = "before"
  inputs = { "a" = "o:::dataset:1" }
  stage { pipeline = "filter true" }
}

resource "observe_dataset" "target" {
  name   = "target"
  inputs = { "a" = "o:::dataset:2" }
  stage { pipeline = "filter true" }
}

resource "observe_dataset" "after" {
  name   = "after"
  inputs = { "a" = "o:::dataset:3" }
  stage { pipeline = "filter true" }
}
`
	path := filepath.Join(dir, "multi.tf")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	_, datasets, _, err := scanExistingResources(dir)
	if err != nil {
		t.Fatal(err)
	}
	target, ok := datasets["target"]
	if !ok {
		t.Fatal("want the target dataset found")
	}

	got := src[target.Range.Start.Byte:target.Range.End.Byte]
	if !strings.HasPrefix(got, `resource "observe_dataset" "target"`) {
		t.Errorf("range does not start at target's own resource block:\n%s", got)
	}
	if strings.Contains(got, `"before"`) || strings.Contains(got, `"after"`) {
		t.Errorf("range spills into a neighboring block:\n%s", got)
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), "}") {
		t.Errorf("range does not end at target's own closing brace:\n%s", got)
	}
}
