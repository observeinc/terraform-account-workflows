package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tfdatasetimport/rewrite"
	"tfdatasetimport/tf"
)

// newPlacementTestRepo builds a minimal repo in a temp dir: a root config declaring a module block
// (all planResult needs from LoadRepo besides the Existing* maps) plus the given module-relative
// files. Returns the loaded *tf.Repo.
func newPlacementTestRepo(t *testing.T, files map[string]string) *tf.Repo {
	t.Helper()
	root := t.TempDir()
	const moduleDir = "modules/target"
	if err := os.MkdirAll(filepath.Join(root, moduleDir), 0o755); err != nil {
		t.Fatal(err)
	}
	rootCfg := `module "target" {
  source = "./modules/target"
}
`
	if err := os.WriteFile(filepath.Join(root, "main.tf"), []byte(rootCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, moduleDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	repo, err := tf.LoadRepo(root, moduleDir)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	return repo
}

// groupedFixture mirrors the real-world shape that triggered the bug: a hand-written file
// declaring several thematically grouped datasets under one filename that matches none of their
// resource names. "target" is the one an update in these tests is aimed at; "sibling_a" and
// "sibling_b" exist purely to prove they are left untouched.
const groupedFixture = `resource "observe_dataset" "sibling_a" {
  name   = "Sibling A"
  inputs = { "a" = "o:::dataset:1" }
  stage {
    pipeline = "filter true"
  }
}

resource "observe_dataset" "target" {
  name   = "Target (old)"
  inputs = { "a" = "o:::dataset:2" }
  stage {
    pipeline = "filter true"
  }
}

resource "observe_dataset" "sibling_b" {
  name   = "Sibling B"
  inputs = { "a" = "o:::dataset:3" }
  stage {
    pipeline = "filter true"
  }
}
`

func TestPlanResultNewResourceUsesConvention(t *testing.T) {
	repo := newPlacementTestRepo(t, nil)
	res := &rewrite.Result{DatasetID: "1", ResourceName: "brand_new", HCL: []byte(`resource "observe_dataset" "brand_new" {}`), DatasetHCL: []byte(`resource "observe_dataset" "brand_new" {}`)}

	p, err := planResult(repo, res)
	if err != nil {
		t.Fatal(err)
	}
	if !p.New {
		t.Error("want New for a resource the module does not already declare")
	}
	if want := filepath.Join(repo.ModulePath(), "brand_new.tf"); p.DatasetFile != want {
		t.Errorf("DatasetFile = %q, want %q", p.DatasetFile, want)
	}
	if p.RelFile != filepath.Join("modules/target", "brand_new.tf") {
		t.Errorf("RelFile = %q", p.RelFile)
	}
}

func TestPlanResultAlreadyManagedTargetsExistingFile(t *testing.T) {
	repo := newPlacementTestRepo(t, map[string]string{"grouped.tf": groupedFixture})
	newDatasetHCL := []byte(`resource "observe_dataset" "target" {
  name = "Target (new)"
}
`)
	res := &rewrite.Result{DatasetID: "2", ResourceName: "target", HCL: newDatasetHCL, DatasetHCL: newDatasetHCL}

	p, err := planResult(repo, res)
	if err != nil {
		t.Fatal(err)
	}
	if p.New {
		t.Fatal("want an already-managed dataset placed as an update, not New")
	}
	wantFile := filepath.Join(repo.ModulePath(), "grouped.tf")
	if p.DatasetFile != wantFile {
		t.Errorf("DatasetFile = %q, want %q (the file it is already declared in)", p.DatasetFile, wantFile)
	}
	if len(p.edits) != 1 {
		t.Fatalf("want exactly one edit (no grants), got %d", len(p.edits))
	}
	e := p.edits[0]
	if e.file != wantFile {
		t.Errorf("edit file = %q, want %q", e.file, wantFile)
	}
	got := groupedFixture[e.edit.Start:e.edit.End]
	if !strings.HasPrefix(got, `resource "observe_dataset" "target"`) {
		t.Errorf("edit range does not start at target's own block:\n%s", got)
	}
	if strings.Contains(got, "sibling_a") || strings.Contains(got, "sibling_b") {
		t.Errorf("edit range spills into a sibling resource:\n%s", got)
	}
}

func TestPlanResultInsertsGrantsWhenMissing(t *testing.T) {
	repo := newPlacementTestRepo(t, map[string]string{"grouped.tf": groupedFixture})
	datasetHCL := []byte(`resource "observe_dataset" "target" {
  name = "Target (new)"
}
`)
	grantsHCL := []byte(`resource "observe_resource_grants" "target" {
  oid = observe_dataset.target.oid
}
`)
	res := &rewrite.Result{DatasetID: "2", ResourceName: "target", DatasetHCL: datasetHCL, GrantsHCL: grantsHCL}

	p, err := planResult(repo, res)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.edits) != 2 {
		t.Fatalf("want a dataset-replace edit and a grants-insert edit, got %d edits: %+v", len(p.edits), p.edits)
	}
	datasetEdit, grantsEdit := p.edits[0], p.edits[1]

	if datasetEdit.edit.Start == datasetEdit.edit.End {
		t.Error("the dataset edit should be a replace (non-zero-width), not an insert")
	}
	if grantsEdit.edit.Start != grantsEdit.edit.End {
		t.Errorf("the grants edit should be a zero-width insert since none exists yet, got [%d,%d)",
			grantsEdit.edit.Start, grantsEdit.edit.End)
	}
	if grantsEdit.edit.Start != datasetEdit.edit.End {
		t.Errorf("grants should insert right after the dataset's own block ends (%d), got insert at %d",
			datasetEdit.edit.End, grantsEdit.edit.Start)
	}
	if grantsEdit.file != datasetEdit.file {
		t.Errorf("grants should insert into the dataset's own file when none exists yet: got %q vs %q",
			grantsEdit.file, datasetEdit.file)
	}
}

func TestPlanResultReplacesGrantsWhenAlreadyDeclaredElsewhere(t *testing.T) {
	// The grants block lives in a separate file from the dataset -- an edge case
	// scanExistingResources supports (see TestDatasetsAndGrantsAreIndependentOfScanOrder in the tf
	// package), exercised here end to end through planResult.
	grantsFixture := `resource "observe_resource_grants" "target" {
  oid = observe_dataset.target.oid
  grant {
    subject = "o:::rbacgroup:old"
    role    = "dataset_viewer"
  }
}
`
	repo := newPlacementTestRepo(t, map[string]string{
		"grouped.tf": groupedFixture,
		"rbac.tf":    grantsFixture,
	})
	datasetHCL := []byte(`resource "observe_dataset" "target" {
  name = "Target (new)"
}
`)
	newGrantsHCL := []byte(`resource "observe_resource_grants" "target" {
  oid = observe_dataset.target.oid
  grant {
    subject = "o:::rbacgroup:new"
    role    = "dataset_viewer"
  }
}
`)
	res := &rewrite.Result{DatasetID: "2", ResourceName: "target", DatasetHCL: datasetHCL, GrantsHCL: newGrantsHCL}

	p, err := planResult(repo, res)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.edits) != 2 {
		t.Fatalf("want two edits, got %d", len(p.edits))
	}
	grantsEdit := p.edits[1]
	wantFile := filepath.Join(repo.ModulePath(), "rbac.tf")
	if grantsEdit.file != wantFile {
		t.Errorf("grants edit file = %q, want %q (where the grants block already lives)", grantsEdit.file, wantFile)
	}
	if grantsEdit.edit.Start == grantsEdit.edit.End {
		t.Error("an already-declared grants block should be replaced, not inserted")
	}
	got := grantsFixture[grantsEdit.edit.Start:grantsEdit.edit.End]
	if !strings.HasPrefix(got, `resource "observe_resource_grants" "target"`) {
		t.Errorf("edit range does not start at the grants block:\n%s", got)
	}
}

// TestWriteResourceFilesUpdatesInPlaceWithoutDuplicatingTheResource is the regression test for the bug this change
// fixes: a dataset the module already manages, declared in a file grouping several datasets
// together under a filename that matches none of them (grouped.tf, in the real repo), must be
// updated in place -- not written to a second file named after its own resource name, which would
// declare the same resource address twice and fail terraform validate outright.
func TestWriteResourceFilesUpdatesInPlaceWithoutDuplicatingTheResource(t *testing.T) {
	repo := newPlacementTestRepo(t, map[string]string{"grouped.tf": groupedFixture})
	datasetHCL := []byte(`resource "observe_dataset" "target" {
  name = "Target (new)"
}
`)
	grantsHCL := []byte(`resource "observe_resource_grants" "target" {
  oid = observe_dataset.target.oid
}
`)
	res := &rewrite.Result{DatasetID: "2", ResourceName: "target", DatasetHCL: datasetHCL, GrantsHCL: grantsHCL,
		HCL: append(append([]byte{}, datasetHCL...), grantsHCL...)}

	plans, err := planResults(repo, []*rewrite.Result{res})
	if err != nil {
		t.Fatal(err)
	}
	written, err := writeResourceFiles([]*rewrite.Result{res}, plans)
	if err != nil {
		t.Fatal(err)
	}

	// Exactly one file touched: the existing grouped.tf. No target.tf.
	if len(written) != 1 {
		t.Fatalf("want exactly one file written, got %v", written)
	}
	if bad := filepath.Join(repo.ModulePath(), "target.tf"); written[0] == bad || fileExists(t, bad) {
		t.Errorf("must not create %s -- that is the exact bug this fixes", bad)
	}
	wantFile := filepath.Join(repo.ModulePath(), "grouped.tf")
	if written[0] != wantFile {
		t.Errorf("written[0] = %q, want %q", written[0], wantFile)
	}

	got := readFile(t, wantFile)
	if strings.Contains(got, "Target (old)") {
		t.Errorf("target's old content should have been replaced:\n%s", got)
	}
	if !strings.Contains(got, "Target (new)") {
		t.Errorf("target's new content should be present:\n%s", got)
	}
	if !strings.Contains(got, `resource "observe_resource_grants" "target"`) {
		t.Errorf("the newly-acquired grants block should have been inserted:\n%s", got)
	}
	// The two other datasets in the same file must be byte-for-byte untouched.
	for _, want := range []string{`name   = "Sibling A"`, `name   = "Sibling B"`} {
		if !strings.Contains(got, want) {
			t.Errorf("sibling resource content lost, missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, `resource "observe_dataset" "target"`) != 1 {
		t.Errorf("want exactly one observe_dataset.target block, got:\n%s", got)
	}
}

// TestWriteResourceFilesGroupsEditsToTheSameFile covers two different already-managed results that
// both happen to live in the same hand-written file: both edits must land in a single
// read-modify-write, not two independent writes where the second would work from stale offsets.
func TestWriteResourceFilesGroupsEditsToTheSameFile(t *testing.T) {
	repo := newPlacementTestRepo(t, map[string]string{"grouped.tf": groupedFixture})
	resA := &rewrite.Result{DatasetID: "1", ResourceName: "sibling_a",
		DatasetHCL: []byte(`resource "observe_dataset" "sibling_a" {
  name = "Sibling A (new)"
}
`)}
	resB := &rewrite.Result{DatasetID: "3", ResourceName: "sibling_b",
		DatasetHCL: []byte(`resource "observe_dataset" "sibling_b" {
  name = "Sibling B (new)"
}
`)}

	plans, err := planResults(repo, []*rewrite.Result{resA, resB})
	if err != nil {
		t.Fatal(err)
	}
	written, err := writeResourceFiles([]*rewrite.Result{resA, resB}, plans)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 {
		t.Fatalf("want both edits applied to the single shared file, got %v", written)
	}

	got := readFile(t, filepath.Join(repo.ModulePath(), "grouped.tf"))
	if !strings.Contains(got, "Sibling A (new)") || !strings.Contains(got, "Sibling B (new)") {
		t.Errorf("both edits should be present:\n%s", got)
	}
	if !strings.Contains(got, `name   = "Target (old)"`) {
		t.Errorf("the untouched target resource should be unaffected:\n%s", got)
	}
}

func TestWriteResourceFilesNewResourceStillGetsItsOwnFile(t *testing.T) {
	repo := newPlacementTestRepo(t, nil)
	res := &rewrite.Result{DatasetID: "1", ResourceName: "brand_new",
		HCL: []byte(`resource "observe_dataset" "brand_new" {
  name = "Brand New"
}
`)}

	plans, err := planResults(repo, []*rewrite.Result{res})
	if err != nil {
		t.Fatal(err)
	}
	written, err := writeResourceFiles([]*rewrite.Result{res}, plans)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(repo.ModulePath(), "brand_new.tf")
	if len(written) != 1 || written[0] != want {
		t.Fatalf("written = %v, want [%s]", written, want)
	}
	if got := readFile(t, want); !strings.Contains(got, "Brand New") {
		t.Errorf("new file content wrong:\n%s", got)
	}
}

func TestTouchedFilesCountsDistinctFilesNotResults(t *testing.T) {
	repo := newPlacementTestRepo(t, map[string]string{"grouped.tf": groupedFixture})
	resA := &rewrite.Result{DatasetID: "1", ResourceName: "sibling_a", DatasetHCL: []byte(`resource "observe_dataset" "sibling_a" {}`)}
	resB := &rewrite.Result{DatasetID: "3", ResourceName: "sibling_b", DatasetHCL: []byte(`resource "observe_dataset" "sibling_b" {}`)}
	resNew := &rewrite.Result{DatasetID: "9", ResourceName: "brand_new", HCL: []byte(`resource "observe_dataset" "brand_new" {}`)}

	plans, err := planResults(repo, []*rewrite.Result{resA, resB, resNew})
	if err != nil {
		t.Fatal(err)
	}
	// Two results share grouped.tf, one gets its own new file: 2 distinct files, not 3.
	if got := len(touchedFiles(plans)); got != 2 {
		t.Errorf("touchedFiles count = %d, want 2 (grouped.tf once, brand_new.tf once)", got)
	}
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
