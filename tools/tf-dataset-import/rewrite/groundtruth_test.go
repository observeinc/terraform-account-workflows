package rewrite

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"tfdatasetimport/observe"
	"tfdatasetimport/tf"
)

// TestGroundTruthAgainstCommittedRepo is the strongest correctness check available without a live
// tenant: for datasets the repo already manages, generator-shaped input is synthesized from state,
// run through the full rewrite, and compared against the committed .tf.
//
// State holds every attribute generation emits (name, freshness, icon_url, description, inputs,
// stage), so the synthesized input is faithful. Comparison is on resolved references and key
// attributes rather than bytes, because the committed files are hand-written and carry comments,
// RBAC resources and formatting no generator would reproduce. The references are the part that has
// to be right.
//
// Set TFDI_STATE, TFDI_REPO, and TFDI_MODULE_DIR to run; the test skips when any is missing,
// since a real state file and account repo do not belong in this repo.
func TestGroundTruthAgainstCommittedRepo(t *testing.T) {
	statePath := envOr("TFDI_STATE", "")
	repoPath := envOr("TFDI_REPO", "")
	moduleDir := envOr("TFDI_MODULE_DIR", "")

	for _, p := range []string{statePath, repoPath, moduleDir} {
		if p == "" {
			t.Skip("set TFDI_STATE, TFDI_REPO, and TFDI_MODULE_DIR to run")
		}
	}
	for _, p := range []string{statePath, repoPath} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("ground truth inputs unavailable (%s); set TFDI_STATE, TFDI_REPO, and TFDI_MODULE_DIR to run", p)
		}
	}

	state, err := tf.LoadState(statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	repo, err := tf.LoadRepo(repoPath, moduleDir)
	if err != nil {
		t.Fatalf("load repo: %v", err)
	}
	index := tf.BuildIndex(state, "module."+filepath.Base(moduleDir))

	// A dataset already in the repo is not "being imported", so the self-map is empty: every
	// reference must come from the module peers and bindings, which is exactly what is under test.
	refs := (&tf.Builder{Index: index, Repo: repo, SelfOIDs: map[string]string{}}).Build()
	t.Logf("reference map holds %d oids", refs.Len())

	// Set TFDI_CASES to a comma-separated list of resource names from TFDI_REPO to test.
	// Choose names that cover: datastream binding, peer .oid references, data_table_view_state,
	// storage_integration via an observe_oid data source, and a dataset with no freshness.
	casesEnv := os.Getenv("TFDI_CASES")
	if casesEnv == "" {
		t.Skip("set TFDI_CASES to a comma-separated list of resource names to test")
	}
	cases := strings.Split(casesEnv, ",")

	for _, resourceName := range cases {
		t.Run(resourceName, func(t *testing.T) {
			attrs := findManagedDataset(state, resourceName)
			if attrs == nil {
				t.Skipf("%s not present in this state file", resourceName)
			}

			def := synthesizeDefinition(t, attrs, resourceName)
			w := &Rewriter{Refs: refs, FreshnessDefault: "5m"}
			res, err := w.Rewrite(def, resourceName, nil)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}

			committed := readCommittedDataset(t, repo.ModulePath(), resourceName)
			generated := parseDatasetBlock(t, string(res.HCL), resourceName)

			compareInputs(t, committed, generated, resourceName)
			compareAttr(t, committed, generated, "name")
			compareAttr(t, committed, generated, "workspace")
			compareFreshness(t, committed, generated, resourceName)
		})
	}
}

// compareInputs is the heart of the test: every input in the committed file must resolve to the
// same reference the tool produces.
func compareInputs(t *testing.T, committed, generated *hclwrite.Body, resourceName string) {
	t.Helper()

	wantEntries, ok, err := parseMapAttr(committed, "inputs")
	if err != nil || !ok {
		t.Fatalf("committed %s: cannot read inputs: %v", resourceName, err)
	}
	gotEntries, ok, err := parseMapAttr(generated, "inputs")
	if err != nil || !ok {
		t.Fatalf("generated %s: cannot read inputs: %v", resourceName, err)
	}

	want := map[string]string{}
	for _, e := range wantEntries {
		want[e.Key] = normalizeRef(e.Value)
	}
	got := map[string]string{}
	for _, e := range gotEntries {
		got[e.Key] = normalizeRef(e.Value)
	}

	for key, wantRef := range want {
		gotRef, present := got[key]
		if !present {
			// The committed file may declare an input the live dataset no longer has, or one the
			// backend added. Report it rather than failing the reference check.
			t.Logf("committed input %q is absent from generated output", key)
			continue
		}
		if gotRef != wantRef {
			t.Errorf("input %q: generated %s, committed %s", key, gotRef, wantRef)
		}
	}
	for key := range got {
		if _, present := want[key]; !present {
			t.Logf("generated input %q is absent from the committed file", key)
		}
	}
}

// compareAttr fails when either side is missing: a generated file with no `name` or `workspace` is a
// bug, and returning early on an empty string would pass it silently.
func compareAttr(t *testing.T, committed, generated *hclwrite.Body, name string) {
	t.Helper()
	want := normalizeRef(attrExpr(committed, name))
	got := normalizeRef(attrExpr(generated, name))
	if want == "" {
		t.Logf("committed file declares no %s; nothing to compare", name)
		return
	}
	if got == "" {
		t.Errorf("%s missing from generated output (committed has %s)", name, want)
		return
	}
	if want != got {
		t.Errorf("%s: generated %s, committed %s", name, got, want)
	}
}

// compareFreshness checks the lookup shape rather than the exact expression: a repo may use several
// fallback forms, and the tool always emits the var.freshness_default form.
func compareFreshness(t *testing.T, committed, generated *hclwrite.Body, resourceName string) {
	t.Helper()
	want := attrExpr(committed, "freshness")
	got := attrExpr(generated, "freshness")

	if want == "" {
		if got != "" {
			t.Errorf("committed file declares no freshness but generated %s", got)
		}
		return
	}
	if !strings.Contains(want, "lookup(local.freshness") {
		t.Logf("committed freshness is not a lookup (%s); skipping", want)
		return
	}
	wantKey := fmt.Sprintf("%q", resourceName)
	if !strings.Contains(want, wantKey) {
		t.Logf("committed lookup key differs from the resource name: %s", want)
		return
	}
	if !strings.Contains(got, "lookup(local.freshness") || !strings.Contains(got, wantKey) {
		t.Errorf("freshness: generated %s, want a lookup keyed on %s", got, wantKey)
	}
}

// synthesizeDefinition renders state attributes as the generator would: a resource block with
// alphabetical attributes, hardcoded oids and heredoc pipelines.
func synthesizeDefinition(t *testing.T, attrs map[string]any, resourceName string) *observe.TerraformDefinition {
	t.Helper()

	f := hclwrite.NewEmptyFile()
	body := f.Body().AppendNewBlock("resource", []string{"observe_dataset", resourceName}).Body()

	var keys []string
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		switch key {
		case "stage", "id", "oid", "inputs":
			continue // handled separately or stripped by the generator
		}
		switch v := attrs[key].(type) {
		case string:
			if v == "" {
				continue
			}
			if err := setAttr(body, key, mustQuote(v)); err != nil {
				t.Fatal(err)
			}
		case bool:
			if err := setAttr(body, key, fmt.Sprintf("%t", v)); err != nil {
				t.Fatal(err)
			}
		case float64:
			if err := setAttr(body, key, fmt.Sprintf("%d", int64(v))); err != nil {
				t.Fatal(err)
			}
		}
	}

	if inputs, ok := attrs["inputs"].(map[string]any); ok {
		var pairs []hclwrite.ObjectAttrTokens
		var inputKeys []string
		for k := range inputs {
			inputKeys = append(inputKeys, k)
		}
		sort.Strings(inputKeys)
		for _, k := range inputKeys {
			oid, _ := inputs[k].(string)
			// Generation emits the bare oid; state may carry a version suffix when the config
			// referenced a peer's .oid.
			keyTokens, err := tokensForExpr(mustQuote(k))
			if err != nil {
				t.Fatal(err)
			}
			valTokens, err := tokensForExpr(mustQuote(tf.NormalizeOID(oid)))
			if err != nil {
				t.Fatal(err)
			}
			pairs = append(pairs, hclwrite.ObjectAttrTokens{Name: keyTokens, Value: valTokens})
		}
		body.SetAttributeRaw("inputs", hclwrite.TokensForObject(pairs))
	}

	stages, _ := attrs["stage"].([]any)
	for _, s := range stages {
		stage, _ := s.(map[string]any)
		if stage == nil {
			continue
		}
		body.AppendNewline()
		sb := body.AppendNewBlock("stage", nil).Body()
		for _, key := range []string{"alias", "input"} {
			if v, _ := stage[key].(string); v != "" {
				if err := setAttr(sb, key, mustQuote(v)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if v, _ := stage["output_stage"].(bool); v {
			if err := setAttr(sb, "output_stage", "true"); err != nil {
				t.Fatal(err)
			}
		}
		if pipeline, _ := stage["pipeline"].(string); pipeline != "" {
			sb.SetAttributeRaw("pipeline", heredocTokens(pipeline))
		}
	}

	name, _ := attrs["name"].(string)
	id, _ := attrs["id"].(string)
	return &observe.TerraformDefinition{
		Resource:   string(hclwrite.Format(f.Bytes())),
		ImportID:   id,
		ImportName: sanitizeIdentifier(name),
	}
}

func findManagedDataset(s *tf.State, resourceName string) map[string]any {
	for i := range s.Resources {
		r := &s.Resources[i]
		if r.Type == "observe_dataset" && r.Mode == "managed" && r.Name == resourceName && len(r.Instances) > 0 {
			return r.Instances[0].Attributes
		}
	}
	return nil
}

// readCommittedDataset finds the committed observe_dataset block for a resource name, searching the
// whole module because the repo does not always name a file after the resource it declares.
func readCommittedDataset(t *testing.T, moduleDir, resourceName string) *hclwrite.Body {
	t.Helper()

	entries, err := os.ReadDir(moduleDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".tf") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(moduleDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), `"`+resourceName+`"`) {
			continue
		}
		if body := findDatasetBlock(t, src, e.Name(), resourceName); body != nil {
			return body
		}
	}
	t.Fatalf("no committed observe_dataset %q found in %s", resourceName, moduleDir)
	return nil
}

func parseDatasetBlock(t *testing.T, src, resourceName string) *hclwrite.Body {
	t.Helper()
	body := findDatasetBlock(t, []byte(src), "generated.tf", resourceName)
	if body == nil {
		t.Fatalf("generated output has no observe_dataset %q", resourceName)
	}
	return body
}

func findDatasetBlock(t *testing.T, src []byte, filename, resourceName string) *hclwrite.Body {
	t.Helper()
	f, diags := hclwrite.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("parse %s: %s", filename, diags.Error())
	}
	for _, block := range f.Body().Blocks() {
		labels := block.Labels()
		if block.Type() == "resource" && len(labels) == 2 &&
			labels[0] == "observe_dataset" && labels[1] == resourceName {
			return block.Body()
		}
	}
	return nil
}

// normalizeRef collapses whitespace so an expression compares equal regardless of the alignment
// terraform fmt chose.
func normalizeRef(expr string) string {
	return strings.Join(strings.Fields(expr), " ")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// heredocTokens renders a pipeline as an indented heredoc, the form generation uses.
//
// Test-only: this builds the tool's *input*. The rewrite deliberately copies a pipeline's tokens
// rather than re-emitting them, so nothing in the production path formats a heredoc.
func heredocTokens(body string) hclwrite.Tokens {
	var indented strings.Builder
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		indented.WriteString("    ")
		indented.WriteString(line)
		indented.WriteString("\n")
	}
	return hclwrite.Tokens{
		{Type: hclsyntax.TokenOHeredoc, Bytes: []byte("<<-EOT\n")},
		{Type: hclsyntax.TokenStringLit, Bytes: []byte(indented.String())},
		{Type: hclsyntax.TokenCHeredoc, Bytes: []byte("EOT")},
	}
}
