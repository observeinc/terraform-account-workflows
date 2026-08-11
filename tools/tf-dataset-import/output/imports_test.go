package output

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testManifest() *Manifest {
	return &Manifest{
		CustomerID:    "102",
		Domain:        "observeinc.com",
		ModuleDir:     "modules/example",
		ModuleAddress: "module.example",
		TFWorkspace:   "default",
		Entries: []Entry{
			{SourceDatasetID: "99005906", ResourceName: "example_events",
				ImportAddress: "module.example.observe_dataset.example_events"},
			{SourceDatasetID: "99006012", ResourceName: "example_metrics",
				ImportAddress: "module.example.observe_dataset.example_metrics"},
		},
	}
}

func writeBlocks(t *testing.T, gate string) string {
	t.Helper()
	got, _ := writeBlocksFor(t, filepath.Join(t.TempDir(), ImportBlocksFile), testManifest(), gate)
	return got
}

// writeBlocksFor writes m to path and returns the file's content plus how many blocks were written.
func writeBlocksFor(t *testing.T, path string, m *Manifest, gate string) (string, int) {
	t.Helper()
	n, err := WriteImportBlocks(path, m, gate)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), n
}

func testManifestWithGrants() *Manifest {
	m := testManifest()
	m.Entries[0].HasGrants = true
	return m
}

// TestImportBlocksTargetThroughTheModule pins the one thing about these blocks that is not obvious:
// terraform rejects an import block inside a child module, so the target has to be named through the
// module from the root rather than sitting beside the resource it imports.
func TestImportBlocksTargetThroughTheModule(t *testing.T) {
	got := writeBlocks(t, "")
	for _, want := range []string{
		"to = module.example.observe_dataset.example_events",
		`id = "99005906"`,
		"to = module.example.observe_dataset.example_metrics",
		`id = "99006012"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "for_each") {
		t.Errorf("ungated output should not use for_each:\n%s", got)
	}
}

// TestGatedImportBlocksScopeToOneWorkspace covers the multi-tenant case: one root config can serve
// several workspaces, and an ungated block would import the source tenant's ids into all of them.
//
// `to` stays unindexed on purpose. It names a single resource rather than an instance of a for_each
// resource, so there is no key to index it by, and for_each carries the id instead.
func TestGatedImportBlocksScopeToOneWorkspace(t *testing.T) {
	got := writeBlocks(t, "local.in_west")
	for _, want := range []string{
		`for_each = local.in_west ? toset(["99005906"]) : toset([])`,
		"to       = module.example.observe_dataset.example_events",
		"id       = each.value",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The literal id belongs in for_each, never in id, or the gate would not control it.
	if strings.Contains(got, `id       = "99005906"`) {
		t.Errorf("gated block should take its id from each.value:\n%s", got)
	}
}

// TestImportBlocksNameTheVersionFloor guards a footgun: for_each in an import block needs Terraform
// 1.7, and an account repo's CI may well pin something older, in which case the generated file fails
// to parse rather than doing something subtly wrong.
func TestImportBlocksNameTheVersionFloor(t *testing.T) {
	if got := writeBlocks(t, "local.in_west"); !strings.Contains(got, "1.7") {
		t.Errorf("gated output should say it needs Terraform 1.7:\n%s", got)
	}
	// Ungated blocks carry no such floor, so claiming one would send the reader off to change a
	// version they do not need to touch. They get the every-workspace warning instead.
	got := writeBlocks(t, "")
	if strings.Contains(got, "1.7") {
		t.Errorf("ungated output should not claim a 1.7 floor:\n%s", got)
	}
	if !strings.Contains(got, "-import-gate") {
		t.Errorf("ungated output should point at -import-gate:\n%s", got)
	}
}

// TestImportBlocksNothingToImportIsNotAnError covers the update-only case: every entry already
// managed, nothing new to write. Unlike the old fixed-batch design, this is a normal, expected
// outcome — not refused, and no file is created just to hold nothing.
func TestImportBlocksNothingToImportIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    *Manifest
	}{
		{"no entries at all", func() *Manifest { m := testManifest(); m.Entries = nil; return m }()},
		{"entries present but all already managed", func() *Manifest {
			m := testManifest()
			for i := range m.Entries {
				m.Entries[i].AlreadyManaged = true
			}
			return m
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ImportBlocksFile)

			n, err := WriteImportBlocks(path, tc.m, "")
			if err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if n != 0 {
				t.Errorf("want 0 blocks written, got %d", n)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("no file should have been written, stat err = %v", err)
			}
		})
	}
}

// TestImportBlocksIncludeResourceGrants covers a dataset that carries RBAC grants: it needs an import
// block for the grants resource too, addressed and identified separately from the dataset's own.
func TestImportBlocksIncludeResourceGrants(t *testing.T) {
	got, n := writeBlocksFor(t, filepath.Join(t.TempDir(), ImportBlocksFile), testManifestWithGrants(), "")
	if n != 3 {
		t.Errorf("want 3 blocks (dataset+grants for entry 0, dataset for entry 1), got %d", n)
	}
	for _, want := range []string{
		"to = module.example.observe_dataset.example_events",
		`id = "99005906"`,
		"to = module.example.observe_resource_grants.example_events",
		`id = "o:::dataset:99005906"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The second entry has no grants, so it should not get a grants import block.
	if strings.Contains(got, "observe_resource_grants.example_metrics") {
		t.Errorf("entry without grants should not get a grants import block:\n%s", got)
	}
}

func TestGatedImportBlocksIncludeResourceGrants(t *testing.T) {
	got, _ := writeBlocksFor(t, filepath.Join(t.TempDir(), ImportBlocksFile), testManifestWithGrants(), "local.in_west")
	for _, want := range []string{
		`for_each = local.in_west ? toset(["o:::dataset:99005906"]) : toset([])`,
		"to       = module.example.observe_resource_grants.example_events",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestImportBlocksAreTerraformFmtClean guards the same CI gate the dataset files do: the repo runs
// `terraform fmt -check`, and this file lands in the repo root.
func TestImportBlocksAreTerraformFmtClean(t *testing.T) {
	bin, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform not on PATH")
	}
	for _, gate := range []string{"", "local.in_west"} {
		dir := t.TempDir()
		if _, err := WriteImportBlocks(filepath.Join(dir, ImportBlocksFile), testManifestWithGrants(), gate); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(bin, "fmt", "-check", "-diff", dir).CombinedOutput()
		if err != nil {
			t.Errorf("gate %q is not fmt clean:\n%s", gate, out)
		}
	}
}

// TestImportBlocksSkipAlreadyManagedEntries covers the per-sub-resource decision: an entry whose
// dataset is already managed gets no dataset import block, independent of whether its grants
// companion (if it has one) still needs one.
func TestImportBlocksSkipAlreadyManagedEntries(t *testing.T) {
	m := testManifest()
	m.Entries[0].AlreadyManaged = true
	m.Entries[0].HasGrants = true
	// Grants acquired since the dataset was first imported: the dataset itself needs no new import,
	// but the grants resource does.

	got, n := writeBlocksFor(t, filepath.Join(t.TempDir(), ImportBlocksFile), m, "")
	if n != 2 {
		t.Errorf("want 2 blocks (grants for entry 0, dataset for entry 1), got %d", n)
	}
	if strings.Contains(got, "observe_dataset.example_events") {
		t.Errorf("an already-managed dataset should get no import block:\n%s", got)
	}
	if !strings.Contains(got, "observe_resource_grants.example_events") {
		t.Errorf("its not-yet-managed grants companion should still get one:\n%s", got)
	}
	if !strings.Contains(got, "observe_dataset.example_metrics") {
		t.Errorf("the other, genuinely new entry should still get its import block:\n%s", got)
	}
}

// TestImportBlocksAppendToAnExistingFile covers the core of the repeated-runs design: this tool runs
// once per dataset id, indefinitely, against the same repo, so the file has to accumulate rather than
// be overwritten. An existing file's content must survive byte for byte, with new blocks only added
// after it.
func TestImportBlocksAppendToAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ImportBlocksFile)

	first := testManifest()
	first.Entries = first.Entries[:1] // just aws_billing_records, as an earlier run would have seen it
	firstContent, n := writeBlocksFor(t, path, first, "")
	if n != 1 {
		t.Fatalf("want 1 block on the first run, got %d", n)
	}

	second := testManifest()
	second.Entries = second.Entries[1:] // just billing_credit_events, a later run for a different id
	got, n := writeBlocksFor(t, path, second, "")
	if n != 1 {
		t.Fatalf("want 1 new block on the second run, got %d", n)
	}
	if !strings.HasPrefix(got, firstContent) {
		t.Errorf("the first run's content must survive byte for byte at the start of the file:\n--- first ---\n%s\n--- got ---\n%s", firstContent, got)
	}
	if !strings.Contains(got, "observe_dataset.example_metrics") {
		t.Errorf("the second run's block should be appended:\n%s", got)
	}
	// The header is written once, on the file's first creation, and never repeated.
	if n := strings.Count(got, "Generated by tf-dataset-import"); n != 1 {
		t.Errorf("want exactly one header, found %d", n)
	}
}
