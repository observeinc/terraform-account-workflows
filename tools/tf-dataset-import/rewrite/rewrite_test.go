package rewrite

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2/hclwrite"

	"tfdatasetimport/observe"
	"tfdatasetimport/tf"
)

// formatted re-runs hclwrite.Format so two byte slices built by different paths (e.g. one
// concatenated from pieces, one built as a whole) compare equal on content despite incidental
// whitespace differences at the seam.
func formatted(t *testing.T, b []byte) string {
	t.Helper()
	return string(hclwrite.Format(b))
}

const (
	fakeRepo      = "../testdata/fake-repo"
	fakeState     = "../testdata/fake-repo-state.json"
	goldenDir     = "../testdata/golden"
	moduleDir     = "modules/example"
	moduleAddress = "module.example"
)

// harness wires the real packages together over the fake repo, so a rewrite test exercises the
// same reference resolution the binary does.
type harness struct {
	t    *testing.T
	repo *tf.Repo
	refs *tf.Map
	defs map[string]*observe.TerraformDefinition
}

func newHarness(t *testing.T, datasetIDs ...string) *harness {
	t.Helper()

	repo, err := tf.LoadRepo(fakeRepo, moduleDir)
	if err != nil {
		t.Fatalf("load fake repo: %v", err)
	}
	state, err := tf.LoadState(fakeState)
	if err != nil {
		t.Fatalf("load fake state: %v", err)
	}

	source := &observe.DirSource{Dir: goldenDir}
	defs := map[string]*observe.TerraformDefinition{}
	for _, id := range datasetIDs {
		def, err := source.GetDatasetTerraform(context.Background(), id)
		if err != nil {
			t.Fatalf("fixture %s: %v", id, err)
		}
		defs[id] = def
	}

	// Built before AssignNames, as the binary does: the identity check that decides "already managed"
	// reads it.
	index := tf.BuildIndex(state, moduleAddress)

	names, _, collisions := AssignNames(defs, repo, index, nil)
	if len(collisions) > 0 {
		// Collisions are exercised by their own test; a harness caller expects clean names.
		t.Fatalf("unexpected name collisions: %v", collisions)
	}

	self := map[string]string{}
	for id, name := range names {
		self["o:::dataset:"+id] = name
	}
	refs := (&tf.Builder{
		Index:    index,
		Repo:     repo,
		SelfOIDs: self,
	}).Build()

	return &harness{t: t, repo: repo, refs: refs, defs: defs}
}

// rewrite runs the default configuration: a dataset with no freshness gets a commented-out lookup.
func (h *harness) rewrite(id, name string) *Result {
	h.t.Helper()
	res, err := (&Rewriter{Refs: h.refs, FreshnessDefault: "5m"}).Rewrite(h.defs[id], name, nil)
	if err != nil {
		h.t.Fatalf("rewrite %s: %v", id, err)
	}
	return res
}

func TestRewriteResolvesReferences(t *testing.T) {
	h := newHarness(t, "99010001", "99010002")
	res := h.rewrite("99010001", "new_one")
	got := string(res.HCL)

	assertContains(t, got,
		`workspace = var.workspace.oid`,
		`name = format(var.name_format, "Fake/New Dataset One")`,
		`freshness = lookup(local.freshness, "new_one", var.freshness_default)`,
		`"applogs" = var.applogs_datastream.dataset`,
		`"peer" = observe_dataset.existing_peer.oid`,
		`"sibling" = observe_dataset.new_two.oid`,
		// No reference exists, so the raw oid stays put and the report flags it.
		`"missing_thing" = "o:::dataset:99999999"`,
	)
}

// TestRewriteDropsOnlyBlockedAttributes pins the tool's scope. Only attributes a resource cannot
// accept are removed; everything else is passed through, including values that equal their default,
// because copying is always safe and deciding otherwise means tracking the provider's schema.
func TestRewriteDropsOnlyBlockedAttributes(t *testing.T) {
	h := newHarness(t, "99010001")
	res := h.rewrite("99010001", "new_one")
	got := string(res.HCL)

	for _, blocked := range []string{"acceleration_type", "entity_tags", "on_demand_materialization_length"} {
		if strings.Contains(got, blocked) {
			t.Errorf("blocked attribute %q reached the output:\n%s", blocked, got)
		}
	}

	// Deliberately kept: each equals its default, so declaring it plans identically, and the repo
	// already declares all of them on some datasets.
	assertContains(t, got,
		"acceleration_disabled = false",
		"object_tags = {}",
		"output_stage = false",
	)
}

// TestRewriteOrdersAttributesLikeTheRepo covers the one thing generated output cannot give us.
// The generator emits attributes alphabetically; the reference repo is emphatic about a different
// order, and unanimous about `inputs` last.
func TestRewriteOrdersAttributesLikeTheRepo(t *testing.T) {
	h := newHarness(t, "99010001", "99010002")

	t.Run("dataset", func(t *testing.T) {
		assertAttrOrder(t, string(h.rewrite("99010001", "new_one").HCL),
			// Named order, then attributes the repo has no convention for, then inputs last.
			"workspace", "name", "icon_url", "description", "freshness",
			"acceleration_disabled", "object_tags", "inputs")
	})

	t.Run("correlation tag", func(t *testing.T) {
		w := &Rewriter{Refs: h.refs, FreshnessDefault: "5m", EmitCorrelationTags: true}
		res, err := w.Rewrite(h.defs["99010001"], "new_one", nil)
		if err != nil {
			t.Fatal(err)
		}
		got := string(res.HCL)
		at := strings.Index(got, `"new_one_trace_id"`)
		if at < 0 {
			t.Fatalf("tag resource missing:\n%s", got)
		}
		assertAttrOrder(t, got[at:], "name", "dataset", "column", "path")
	})

	// A commented-out freshness takes the slot the attribute would have had, so uncommenting it
	// needs no reordering.
	t.Run("commented freshness keeps its slot", func(t *testing.T) {
		got := string(h.rewrite("99010002", "new_two").HCL)
		assertAttrOrder(t, got, "name", "# freshness", "acceleration_disabled", "inputs")
	})
}

// assertAttrOrder checks that each name appears, in order, as an attribute at the start of a line.
//
// Anchoring matters: a bare substring search over the whole file can match inside a pipeline heredoc,
// which would let a wrong order pass. `# freshness` is accepted so the commented-out lookup can be
// located in the same way as a real attribute.
func assertAttrOrder(t *testing.T, body string, attrs ...string) {
	t.Helper()

	positions := map[string]int{}
	for offset, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		name, _, found := strings.Cut(trimmed, "=")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if _, seen := positions[name]; !seen {
			positions[name] = offset
		}
	}

	prev, prevName := -1, ""
	for _, attr := range attrs {
		at, present := positions[attr]
		if !present {
			t.Errorf("%s is not an attribute in the output:\n%s", attr, body)
			continue
		}
		if at <= prev {
			t.Errorf("%s should come after %s:\n%s", attr, prevName, body)
		}
		prev, prevName = at, attr
	}
}

func TestRewritePreservesPipelinesVerbatim(t *testing.T) {
	h := newHarness(t, "99010001")
	res := h.rewrite("99010001", "new_one")
	got := string(res.HCL)

	// Pipelines are arbitrary OPAL. Text that looks like HCL or like an oid must survive
	// untouched, which is why the rewrite copies pipeline tokens rather than reformatting them.
	assertContains(t, got,
		`make_col brace_text:"{ not hcl }", oid_text:"o:::dataset:12345"`,
		`union @sub`,
	)
	if n := strings.Count(got, "stage {"); n != 2 {
		t.Errorf("want 2 stage blocks, got %d:\n%s", n, got)
	}
	assertContains(t, got, `alias = "sub"`)
}

func TestRewriteFreshness(t *testing.T) {
	h := newHarness(t, "99010001", "99010002")

	t.Run("records an override in the repo's short form", func(t *testing.T) {
		res := h.rewrite("99010001", "new_one")
		if res.Freshness != "2m" {
			t.Errorf("want freshness 2m, got %q", res.Freshness)
		}
		if res.FreshnessOverride != "2m" {
			t.Errorf("want override 2m, got %q", res.FreshnessOverride)
		}
		if res.FreshnessSynthesized {
			t.Error("freshness was present; should not be marked synthesized")
		}
	})

	t.Run("commented when the dataset has none", func(t *testing.T) {
		res := h.rewrite("99010002", "new_two")
		assertContains(t, string(res.HCL),
			`# freshness = lookup(local.freshness, "new_two", var.freshness_default)`)
		if !res.FreshnessSynthesized {
			t.Error("want FreshnessSynthesized")
		}
		if res.FreshnessOverride != "" {
			t.Errorf("a dataset with no freshness needs no override, got %q", res.FreshnessOverride)
		}
	})

	t.Run("-adopt-freshness-default emits an active lookup", func(t *testing.T) {
		w := &Rewriter{Refs: h.refs, FreshnessDefault: "5m", AdoptFreshnessDefault: true}
		res, err := w.Rewrite(h.defs["99010002"], "new_two", nil)
		if err != nil {
			t.Fatal(err)
		}
		got := string(res.HCL)
		assertContains(t, got, `freshness = lookup(local.freshness, "new_two"`)
		if strings.Contains(got, "# freshness") {
			t.Errorf("the lookup should not be commented out:\n%s", got)
		}
	})
}

// TestShortDuration covers the one place a duration is reformatted: the freshness_overrides entry,
// whose 500-odd existing values are all written short.
func TestShortDuration(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"2m0s", "2m"},
		{"1h0m0s", "1h"},
		{"5m0s", "5m"},
		{"1s", "1s"},
		{"90m0s", "90m"},
		{"768h0m0s", "768h"},
		{"1m30s", "1m30s"},
		{"not a duration", "not a duration"},
	} {
		if got := shortDuration(tc.in); got != tc.want {
			t.Errorf("shortDuration(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRewriteCorrelationTags(t *testing.T) {
	h := newHarness(t, "99010001")

	// Tags are recorded whether or not they are emitted, so the report can name them either way.
	quiet := h.rewrite("99010001", "new_one")
	if len(quiet.CorrelationTags) != 2 {
		t.Errorf("want 2 correlation tags recorded, got %v", quiet.CorrelationTags)
	}
	if strings.Contains(string(quiet.HCL), "observe_correlation_tag") {
		t.Errorf("tags must not be emitted by default:\n%s", quiet.HCL)
	}

	w := &Rewriter{Refs: h.refs, FreshnessDefault: "5m", EmitCorrelationTags: true}
	res, err := w.Rewrite(h.defs["99010001"], "new_one", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(res.HCL)

	// Tag resources are prefixed with the dataset because the generator names them after the bare
	// tag, which collides as soon as two datasets carry the same tag.
	assertContains(t, got,
		`resource "observe_correlation_tag" "new_one_ip_address"`,
		`resource "observe_correlation_tag" "new_one_trace_id"`,
		`dataset = observe_dataset.new_one.oid`,
		`name = "trace.id"`,
		`path = "trace_identifier"`,
	)
	// The repo uses the bare form, not the generator's resource-prefixed reference.
	if strings.Contains(got, "resource.observe_dataset.") {
		t.Errorf("correlation tag kept the resource. prefix:\n%s", got)
	}
	if len(res.CorrelationTags) != 2 {
		t.Errorf("want 2 correlation tags, got %v", res.CorrelationTags)
	}
}

func TestRewriteKeepsAttributesThatWouldOtherwiseDiff(t *testing.T) {
	h := newHarness(t, "99010002")
	res := h.rewrite("99010002", "new_two")
	got := string(res.HCL)

	// data_table_view_state is Optional and read back on import, so omitting it plans as a
	// removal. acceleration_disabled is non-default here, so it has to be declared too.
	if !strings.Contains(got, "data_table_view_state = jsonencode(") {
		t.Errorf("data_table_view_state must survive:\n%s", got)
	}
	assertContains(t, got, "acceleration_disabled = true")
	// The storage integration oid resolves through an observe_oid data source.
	assertContains(t, got, "storage_integration = data.observe_oid.storage_integration_fake.oid")
}

func TestRewriteBucketsInModuleDataSourceForReview(t *testing.T) {
	h := newHarness(t, "99010002")
	res := h.rewrite("99010002", "new_two")

	var found bool
	for _, ref := range res.Refs {
		if ref.OID == "o:::dataset:99002000" {
			found = true
			if ref.Tier != tf.TierModuleData {
				t.Errorf("want TierModuleData, got %v", ref.Tier)
			}
		}
	}
	if !found {
		t.Fatalf("input oid was not recorded in Refs: %+v", res.Refs)
	}
	if !res.NeedsReview() {
		t.Error("a data-source resolution should require review")
	}
}

func TestAssignNamesReportsCollisions(t *testing.T) {
	repo, err := tf.LoadRepo(fakeRepo, moduleDir)
	if err != nil {
		t.Fatalf("load fake repo: %v", err)
	}
	source := &observe.DirSource{Dir: goldenDir}
	def, err := source.GetDatasetTerraform(context.Background(), "99010003")
	if err != nil {
		t.Fatal(err)
	}
	defs := map[string]*observe.TerraformDefinition{"99010003": def}

	names, _, collisions := AssignNames(defs, repo, nil, nil)
	if len(collisions) != 1 {
		t.Fatalf("want 1 collision, got %d: %v", len(collisions), collisions)
	}
	if _, assigned := names["99010003"]; assigned {
		t.Error("a colliding dataset must not be assigned a name")
	}
	c := collisions[0]
	if c.Suggestion == "" || c.Suggestion == c.Name {
		t.Errorf("collision needs a usable suggestion, got %q", c.Suggestion)
	}

	// An explicit override settles it.
	names, _, collisions = AssignNames(defs, repo, nil, map[string]string{"99010003": "existing_peer_clone"})
	if len(collisions) != 0 {
		t.Errorf("override should clear the collision, got %v", collisions)
	}
	if names["99010003"] != "existing_peer_clone" {
		t.Errorf("want the override name, got %q", names["99010003"])
	}
}

// TestSanitizeIdentifierMatchesResolver compares against the resolver's rule
// (`terraform/resolver/helpers.go`), not against a convenient subset of it. The hyphen and
// punctuation-run cases are the ones that matter: a hyphen is valid in a terraform identifier, so
// replacing it would make `trace.id` and `trace-id` collide on one resource name.
func TestSanitizeIdentifierMatchesResolver(t *testing.T) {
	for _, tc := range []struct{ in, wantShort, wantFull string }{
		{"App Logs/Card Authorization", "card_authorization", "app_logs_card_authorization"},
		{"observe/Dataset", "dataset", "observe_dataset"},
		{"3P Vendor", "_3p_vendor", "_3p_vendor"},
		{"Simple", "simple", "simple"},
		// A hyphen is valid and must survive.
		{"my-dataset", "my-dataset", "my-dataset"},
		{"EC2/Instance-Level Metrics", "instance-level_metrics", "ec2_instance-level_metrics"},
		// A run of invalid characters collapses to one underscore.
		{"Foo (bar)", "foo_bar_", "foo_bar_"},
		{"Host — Metrics", "host_metrics", "host_metrics"},
		{"a  b", "a_b", "a_b"},
	} {
		if got := sanitizeIdentifier(tc.in); got != tc.wantShort {
			t.Errorf("sanitizeIdentifier(%q) = %q, want %q", tc.in, got, tc.wantShort)
		}
		if got := sanitizeFullPath(tc.in); got != tc.wantFull {
			t.Errorf("sanitizeFullPath(%q) = %q, want %q", tc.in, got, tc.wantFull)
		}
	}
}

// TestRewriteRejectsCollidingCorrelationTags covers the consequence of two tag names sanitizing to one
// identifier: terraform rejects a duplicate resource for the whole module, so the tool must refuse
// rather than emit it.
//
// Matching the resolver's rule removed the common case — `trace.id` and `trace-id` now differ, because
// a hyphen is valid in an identifier — but `trace.id` and `trace id` still both become `trace_id`.
func TestRewriteRejectsCollidingCorrelationTags(t *testing.T) {
	h := newHarness(t, "99010001")

	def := *h.defs["99010001"]
	def.Resource = strings.Replace(def.Resource, `name = "ip_address"`, `name = "trace id"`, 1)

	w := &Rewriter{Refs: h.refs, FreshnessDefault: "5m"}
	if _, err := w.Rewrite(&def, "new_one", nil); err == nil {
		t.Error("want an error when two tags sanitize to the same resource name")
	} else if !strings.Contains(err.Error(), "sanitize") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// TestRewriteRealGeneratedOutput runs the rewrite over representative provider output, so
// the parser is exercised against the format the provider actually emits rather than only
// against hand-written fixtures.
func TestRewriteRealGeneratedOutput(t *testing.T) {
	h := newHarness(t)
	source := &observe.DirSource{Dir: goldenDir}

	for _, tc := range []struct{ id, name string }{
		{"99010004", "github_events"},
		{"99010005", "correlation_tags"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			def, err := source.GetDatasetTerraform(context.Background(), tc.id)
			if err != nil {
				t.Fatal(err)
			}
			w := &Rewriter{Refs: h.refs, FreshnessDefault: "5m"}
			res, err := w.Rewrite(def, tc.name, nil)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if !strings.Contains(string(res.HCL), "stage {") {
				t.Errorf("no stage block emitted:\n%s", res.HCL)
			}
			// These oids belong to another tenant, so none resolve; the point is that the raw
			// oid is preserved rather than dropped.
			if len(res.Refs) == 0 {
				t.Error("no references recorded")
			}
		})
	}
}

// TestOutputIsTerraformFmtClean guards the CI gate: the repo runs `terraform fmt -check`.
//
// The representative fixtures matter more than the hand-written ones here: 99010005 carries trailing
// whitespace and a whitespace-only line inside a pipeline, which is exactly the content that makes
// re-indenting a heredoc unsafe.
func TestOutputIsTerraformFmtClean(t *testing.T) {
	bin, err := exec_LookPath("terraform")
	if err != nil {
		t.Skip("terraform not on PATH")
	}

	h := newHarness(t, "99010001", "99010002")
	source := &observe.DirSource{Dir: goldenDir}
	dir := t.TempDir()

	for _, tc := range []struct {
		id, name string
		grants   []Grant
	}{
		{"99010001", "new_one", []Grant{{GroupName: "TEST-VIEWER", Role: "dataset_viewer"}}},
		{"99010002", "new_two", nil},
		{"99010004", "github_events", nil},
		{"99010005", "correlation_tags", nil},
	} {
		def, err := source.GetDatasetTerraform(context.Background(), tc.id)
		if err != nil {
			t.Fatal(err)
		}
		// Tags on too, so the tag blocks are covered by the fmt gate as well.
		w := &Rewriter{Refs: h.refs, FreshnessDefault: "5m", EmitCorrelationTags: true}
		res, err := w.Rewrite(def, tc.name, tc.grants)
		if err != nil {
			t.Fatalf("rewrite %s: %v", tc.id, err)
		}
		if err := os.WriteFile(filepath.Join(dir, tc.name+".tf"), res.HCL, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runCmd(bin, "fmt", "-check", "-diff", dir)
	if err != nil {
		t.Errorf("generated output is not terraform fmt clean:\n%s", out)
	}
}

// squashSpaces collapses runs of spaces so assertions do not depend on the column alignment
// `terraform fmt` chooses, which shifts with the longest attribute name in the block.
var multiSpace = regexp.MustCompile(`[ \t]+`)

func squashSpaces(s string) string { return multiSpace.ReplaceAllString(s, " ") }

// assertContains checks for an attribute or fragment ignoring alignment whitespace.
func assertContains(t *testing.T, body string, wants ...string) {
	t.Helper()
	got := squashSpaces(body)
	for _, want := range wants {
		if !strings.Contains(got, squashSpaces(want)) {
			t.Errorf("missing %q in output:\n%s", want, body)
		}
	}
}

func TestResourceGrantsAreEmitted(t *testing.T) {
	h := newHarness(t, "99010001")
	grants := []Grant{
		{GroupName: "TEST-VIEWER", Role: "dataset_viewer"},
		{GroupName: "TEST-WRITER", Role: "dataset_viewer"},
	}
	w := &Rewriter{Refs: h.refs, FreshnessDefault: "5m"}
	res, err := w.Rewrite(h.defs["99010001"], "new_one", grants)
	if err != nil {
		t.Fatal(err)
	}
	got := string(res.HCL)

	assertContains(t, got,
		`resource "observe_resource_grants" "new_one"`,
		`oid = observe_dataset.new_one.oid`,
		`subject = var.rbac_groups["TEST-VIEWER"].oid`,
		`role = "dataset_viewer"`,
		`subject = var.rbac_groups["TEST-WRITER"].oid`,
	)
}

func TestRawSubjectGrantEmitsLiteralWithTODO(t *testing.T) {
	h := newHarness(t, "99010001")
	grants := []Grant{
		{GroupName: "TEST-VIEWER", Role: "dataset_viewer"},
		{RawSubject: "o:::rbacgroup:o::100000000000:rbacgroup:9000000001", Role: "dataset_viewer"},
	}
	w := &Rewriter{Refs: h.refs, FreshnessDefault: "5m"}
	res, err := w.Rewrite(h.defs["99010001"], "new_one", grants)
	if err != nil {
		t.Fatal(err)
	}
	got := string(res.HCL)

	// Resolved grant uses the reference form.
	assertContains(t, got, `subject = var.rbac_groups["TEST-VIEWER"].oid`)
	// Unresolved grant emits the raw OID as a string literal with a TODO comment.
	assertContains(t, got, `# TODO: group o:::rbacgroup:o::100000000000:rbacgroup:9000000001 is not in var.rbac_groups`)
	assertContains(t, got, `subject = "o:::rbacgroup:o::100000000000:rbacgroup:9000000001"`)
}

// TestDatasetAndGrantsHCLAreAddressableSeparately covers why an update needs more than HCL: it has
// to be able to splice just the dataset's own block into an existing file without disturbing
// whatever else that file declares, and independently decide whether the grants block needs
// inserting or already exists. DatasetHCL and GrantsHCL are exactly the bytes HCL is built from —
// this pins that splitting them out changes nothing about what gets emitted, only how it is
// addressed.
func TestDatasetAndGrantsHCLAreAddressableSeparately(t *testing.T) {
	h := newHarness(t, "99010001")
	grants := []Grant{{GroupName: "TEST-VIEWER", Role: "dataset_viewer"}}
	w := &Rewriter{Refs: h.refs, FreshnessDefault: "5m"}
	res, err := w.Rewrite(h.defs["99010001"], "new_one", grants)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.DatasetHCL) == 0 {
		t.Fatal("want a non-empty DatasetHCL")
	}
	if len(res.GrantsHCL) == 0 {
		t.Fatal("want a non-empty GrantsHCL when grants are present")
	}
	assertContains(t, string(res.DatasetHCL), `resource "observe_dataset" "new_one"`)
	if strings.Contains(string(res.DatasetHCL), "observe_resource_grants") {
		t.Errorf("DatasetHCL must not carry the grants block:\n%s", res.DatasetHCL)
	}
	assertContains(t, string(res.GrantsHCL), `resource "observe_resource_grants" "new_one"`)
	// The grants block legitimately references observe_dataset.new_one.oid — only a second
	// resource block would mean the dataset got duplicated into GrantsHCL.
	if strings.Contains(string(res.GrantsHCL), `resource "observe_dataset"`) {
		t.Errorf("GrantsHCL must not carry the dataset block:\n%s", res.GrantsHCL)
	}

	// The split is exhaustive and non-lossy: concatenating the pieces back together reproduces HCL,
	// modulo the blank-line separator HCL inserts between blocks (hclwrite.Format is idempotent on
	// whitespace, so formatting the concatenation again must match formatting HCL again).
	rejoined := append(append([]byte{}, res.DatasetHCL...), append([]byte("\n"), res.GrantsHCL...)...)
	if got, want := formatted(t, rejoined), formatted(t, res.HCL); got != want {
		t.Errorf("DatasetHCL+GrantsHCL does not reproduce HCL:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestDatasetHCLWithNoGrants covers the common case: a dataset with no grants gets a DatasetHCL and
// no GrantsHCL, and DatasetHCL alone matches HCL exactly (nothing else is emitted for it).
func TestDatasetHCLWithNoGrants(t *testing.T) {
	h := newHarness(t, "99010001")
	res := h.rewrite("99010001", "new_one")

	if len(res.GrantsHCL) != 0 {
		t.Errorf("want no GrantsHCL when the dataset has no grants, got:\n%s", res.GrantsHCL)
	}
	if string(res.DatasetHCL) != string(res.HCL) {
		t.Errorf("DatasetHCL should equal HCL when nothing else is emitted:\ngot:\n%s\nwant:\n%s",
			res.DatasetHCL, res.HCL)
	}
}

func TestDataTableViewStateLandsAfterStages(t *testing.T) {
	h := newHarness(t, "99010002")
	res := h.rewrite("99010002", "new_two")
	got := string(res.HCL)

	stageIdx := strings.Index(got, "stage {")
	dtvsIdx := strings.Index(got, "data_table_view_state")
	if stageIdx < 0 || dtvsIdx < 0 {
		t.Fatalf("expected both stage and data_table_view_state in output:\n%s", got)
	}
	if dtvsIdx < stageIdx {
		t.Errorf("data_table_view_state (pos %d) should come after stage blocks (pos %d):\n%s",
			dtvsIdx, stageIdx, got)
	}
}
