package rewrite

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"tfdatasetimport/observe"
	"tfdatasetimport/tf"
)

// blockedAttrs must not reach the config. Everything else the generator emits is passed through
// untouched, because copying is always the safe choice: an attribute that is read back on import has
// to be declared or it plans as a removal, and one that equals its default plans identically either
// way. Only an attribute that *cannot* be declared needs naming here.
var blockedAttrs = map[string]string{
	"id":  "computed resource identity",
	"oid": "computed resource identity",
	// Computed on the resource schema, so declaring it is a hard error rather than a diff.
	"acceleration_type": "computed, not settable",
	// Optional+Computed with diffSuppressAlways, and deprecated. State carries a server-populated
	// value on many datasets while no config declares it, and it can never diff.
	"on_demand_materialization_length": "deprecated, diff-suppressed always",
	// Write-only: import never populates it, so a declaration turns a clean no-op into an update
	// that recomputes the oid of every downstream dependent.
	"rematerialization_mode": "write-only, never read back",
	// Deprecated and ConflictsWith object_tags on the resource schema. getTerraform renders from
	// the data source schema where both are Computed and non-conflicting, so both come through;
	// declaring both on the resource is a hard error. object_tags is the non-deprecated one and
	// is never read back on import, so the empty map it emits does not diff.
	"entity_tags": "deprecated, conflicts with object_tags on resource schema",
}

// Attribute order for observe_dataset. Generated output is alphabetical; the reference repo is not,
// and it is emphatic about it — dataset blocks in a reference account repo agree on this sequence, every pair at
// 98.6% or better, and on `inputs` coming last. Anything not named here sits between the two groups.
//
// data_table_view_state lands after the stage blocks: it is a JSON blob of UI state that dwarfs
// the meaningful config and makes pipelines harder to find when placed among the attributes.
var (
	datasetAttrsFirst       = []string{"workspace", "name", "icon_url", "description", "freshness"}
	datasetAttrsLast        = []string{"inputs"}
	datasetAttrsAfterStages = []string{"data_table_view_state"}
)

// correlationTagAttrs is the order tag blocks in a reference account repo use.
var correlationTagAttrs = []string{"name", "dataset", "column", "path"}

// Grant is one RBAC group grant to emit in an observe_resource_grants block.
//
// Exactly one of GroupName or RawSubject is set:
//   - GroupName is set when the group is in the state's observe_rbac_group resources;
//     the subject is emitted as var.rbac_groups["NAME"].oid.
//   - RawSubject is set when the group is unknown; the subject is emitted as a literal
//     OID string with a TODO comment so the operator can replace it once the group is
//     wired through var.rbac_groups.
type Grant struct {
	GroupName  string // resolved: emit var.rbac_groups["NAME"].oid
	RawSubject string // unresolved: emit as string literal with TODO comment
	Role       string // terraform role name, e.g. "dataset_viewer"
}

// Result is one rewritten dataset.
type Result struct {
	DatasetID    string
	DatasetName  string
	ResourceName string
	HCL          []byte

	// Freshness is the dataset's own value, empty when it has none.
	Freshness string
	// FreshnessSynthesized is true when the dataset has no freshness and the tool emitted a
	// commented-out or live lookup rather than the real value.
	FreshnessSynthesized bool
	// FreshnessOverride is the entry to add to the root freshness_overrides map, if any.
	FreshnessOverride string

	// CorrelationTags are the correlation tag resource names, recorded whether or not they were
	// emitted so the report can name them either way.
	CorrelationTags []string
	// UnknownResources names resource types the generator emitted that this tool has no handling
	// for, and therefore dropped.
	UnknownResources []string
	// Refs records every oid encountered and how it resolved.
	Refs []tf.Ref
}

// NeedsReview reports whether any reference in this dataset blocks a clean exit.
func (r *Result) NeedsReview() bool {
	for _, ref := range r.Refs {
		if ref.Tier.NeedsReview() {
			return true
		}
	}
	return false
}

// Rewriter turns generated definitions into repo-conventional HCL.
type Rewriter struct {
	Refs *tf.Map
	// AdoptFreshnessDefault emits an active freshness lookup for a dataset that has none, rather
	// than a commented-out one.
	AdoptFreshnessDefault bool
	// FreshnessDefault is the module's default, used to skip redundant override entries.
	FreshnessDefault string
	// EmitCorrelationTags writes an observe_correlation_tag resource per tag.
	//
	// Off by default, and the default is the safe one. `observe_correlation_tag` declares no
	// Importer, so `terraform import` rejects it outright and the tags cannot be brought into state
	// alongside their datasets. That leaves them as pending creates — and the backend's
	// AddCorrelationTagTx returns ErrCTagAlreadyExists for a tag that is already on the dataset, so
	// applying them against the tenant being imported from is an error, not a no-op. Tags are named
	// in the report either way.
	EmitCorrelationTags bool
}

// Rewrite converts one generated definition.
//
// Two passes: the generated block's attribute *values* are rewritten in place, then the block is
// copied into the output with its attributes in repo order. The copy moves whole token streams and
// never inspects a value, so an attribute this tool has no opinion about — including one the provider
// adds later — comes through untouched and lands in a defined position.
func (w *Rewriter) Rewrite(def *observe.TerraformDefinition, resourceName string, grants []Grant) (*Result, error) {
	f, diags := hclwrite.ParseConfig([]byte(def.Resource), "generated.tf", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("dataset %s: parse generated terraform: %s", def.ImportID, diags.Error())
	}

	dataset, tags, other := splitResources(f.Body())
	if dataset == nil {
		return nil, fmt.Errorf("dataset %s: generated output has no observe_dataset resource", def.ImportID)
	}

	datasetName, _ := attrString(dataset.Body(), "name")
	res := &Result{
		DatasetID:        def.ImportID,
		DatasetName:      datasetName,
		ResourceName:     resourceName,
		UnknownResources: other,
	}

	freshnessComment, err := w.rewriteDatasetValues(dataset.Body(), res)
	if err != nil {
		return nil, fmt.Errorf("dataset %s: %w", def.ImportID, err)
	}

	// The generator emits correlation tags before the dataset they belong to; the output puts the
	// dataset first.
	out := hclwrite.NewEmptyFile()
	writeDataset(out.Body(), dataset.Body(), resourceName, freshnessComment)

	seen := map[string]bool{}
	for _, tag := range tags {
		tagResource, err := rewriteCorrelationTagValues(tag.Body(), resourceName)
		if err != nil {
			return nil, fmt.Errorf("dataset %s: %w", def.ImportID, err)
		}
		// Two tags whose names sanitize to the same identifier would be a duplicate resource, which
		// terraform rejects for the whole module.
		if seen[tagResource] {
			return nil, fmt.Errorf("dataset %s: two correlation tags both sanitize to %q", def.ImportID, tagResource)
		}
		seen[tagResource] = true

		res.CorrelationTags = append(res.CorrelationTags, tagResource)
		if !w.EmitCorrelationTags {
			continue
		}
		out.Body().AppendNewline()
		writeCorrelationTag(out.Body(), tag.Body(), tagResource)
	}

	if len(grants) > 0 {
		out.Body().AppendNewline()
		writeResourceGrants(out.Body(), resourceName, grants)
	}

	// No generated-file marker: once written, this is meant to look exactly like a dataset a human
	// added to the repo by hand, and re-running for a dataset id already under management is how it
	// stays in sync with Observe going forward regardless of how it first got here.
	res.HCL = hclwrite.Format(out.Bytes())
	return res, nil
}

// rewriteDatasetValues makes the changes the dataset block needs, in place: dropping blocked
// attributes, wrapping the name, and resolving every oid. It returns the freshness lookup to emit as
// a comment, empty when none is wanted.
func (w *Rewriter) rewriteDatasetValues(body *hclwrite.Body, res *Result) (string, error) {
	if !hasBlock(body, "stage") {
		return "", fmt.Errorf("generated output has no stage block")
	}

	for attr := range blockedAttrs {
		body.RemoveAttribute(attr)
	}

	name, present := attrString(body, "name")
	if !present {
		return "", fmt.Errorf("name attribute missing")
	}
	// All datasets in a reference account repo wrap the name this way. name_format defaults to "%s" and is
	// never overridden today, so this plans identically; it exists so a consumer can prefix every
	// dataset in the module, and a dataset that skipped it would be left behind by that.
	if err := setAttr(body, "name", fmt.Sprintf("format(var.name_format, %s)", mustQuote(name))); err != nil {
		return "", err
	}

	for _, attr := range []string{"workspace", "storage_integration"} {
		if err := w.rewriteOIDAttr(body, attr, res); err != nil {
			return "", err
		}
	}
	if err := w.rewriteInputs(body, res); err != nil {
		return "", err
	}
	return w.rewriteFreshness(body, res)
}

// rewriteOIDAttr replaces an attribute holding a single oid with a portable reference. An oid with no
// reference is left in place and reported: the config still applies, it is just pinned to one tenant.
func (w *Rewriter) rewriteOIDAttr(body *hclwrite.Body, name string, res *Result) error {
	oid, present := attrString(body, name)
	if !present || !tf.IsOID(oid) {
		return nil
	}
	ref := w.Refs.Resolve(oid)
	res.Refs = append(res.Refs, ref)
	if ref.Reference == "" {
		return nil
	}
	return setAttr(body, name, ref.Reference)
}

// rewriteInputs resolves each input oid to a reference, preserving key order.
func (w *Rewriter) rewriteInputs(body *hclwrite.Body, res *Result) error {
	entries, ok, err := parseMapAttr(body, "inputs")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("inputs attribute missing or not a map")
	}

	pairs := make([]hclwrite.ObjectAttrTokens, 0, len(entries))
	for _, e := range entries {
		value := e.Value
		if oid, isString := literalString(e.Value); isString && tf.IsOID(oid) {
			ref := w.Refs.Resolve(oid)
			res.Refs = append(res.Refs, ref)
			if ref.Reference != "" {
				value = ref.Reference
			}
		}
		key, err := tokensForExpr(mustQuote(e.Key))
		if err != nil {
			return err
		}
		valueTokens, err := tokensForExpr(value)
		if err != nil {
			return err
		}
		pairs = append(pairs, hclwrite.ObjectAttrTokens{Name: key, Value: valueTokens})
	}

	body.SetAttributeRaw("inputs", hclwrite.TokensForObject(pairs))
	return nil
}

// rewriteFreshness replaces a literal duration with the repo's lookup, whose real value lives in the
// root module block's freshness_overrides map. It returns the lookup to emit as a comment instead,
// when the dataset has no freshness of its own.
func (w *Rewriter) rewriteFreshness(body *hclwrite.Body, res *Result) (string, error) {
	lookup := fmt.Sprintf("lookup(local.freshness, %s, var.freshness_default)", mustQuote(res.ResourceName))

	if raw, present := attrString(body, "freshness"); present {
		res.Freshness = shortDuration(raw)
		// The lookup already falls back to var.freshness_default, so only a value that differs
		// from it needs an entry in the map.
		if res.Freshness != shortDuration(w.FreshnessDefault) {
			res.FreshnessOverride = res.Freshness
		}
		return "", setAttr(body, "freshness", lookup)
	}

	// The dataset has no freshnessDesired. A live lookup would set one, changing materialization
	// cadence and recomputing the oid of every dependent, so the default is a commented line: the
	// post-import plan stays a clean no-op and adopting the convention later is one uncomment.
	res.FreshnessSynthesized = true
	if w.AdoptFreshnessDefault {
		return "", setAttr(body, "freshness", lookup)
	}
	return lookup, nil
}

// rewriteCorrelationTagValues repoints a tag at the renamed dataset and returns the resource name to
// give it.
//
// The generator names the resource after the bare tag, which collides as soon as two datasets carry
// the same tag; writes the dataset reference with a `resource.` prefix the repo never uses; and emits
// `path` as a bare identifier, which terraform would read as a reference to something undeclared.
func rewriteCorrelationTagValues(body *hclwrite.Body, resourceName string) (string, error) {
	tagName, present := attrString(body, "name")
	if !present {
		return "", fmt.Errorf("correlation tag has no name")
	}

	for attr := range blockedAttrs {
		body.RemoveAttribute(attr)
	}
	if err := setAttr(body, "dataset", fmt.Sprintf("observe_dataset.%s.oid", resourceName)); err != nil {
		return "", err
	}
	if _, isString := attrString(body, "path"); !isString && !isNullOrAbsent(body, "path") {
		if err := setAttr(body, "path", mustQuote(strings.TrimSpace(attrExpr(body, "path")))); err != nil {
			return "", err
		}
	}
	return resourceName + "_" + sanitize(strings.ToLower(tagName)), nil
}

// writeDataset appends the dataset resource to dst with its attributes in repo order, followed by the
// stage blocks in source order, and finally any post-stage attributes (like data_table_view_state).
func writeDataset(dst, src *hclwrite.Body, resourceName, freshnessComment string) {
	body := dst.AppendNewBlock("resource", []string{"observe_dataset", resourceName}).Body()

	for _, name := range datasetAttrsFirst {
		copyAttr(body, src, name)
		// The commented-out lookup goes in the slot the attribute would have occupied, so
		// uncommenting it needs no reordering.
		if name == "freshness" && freshnessComment != "" {
			body.AppendUnstructuredTokens(hclwrite.Tokens{{
				Type:  hclsyntax.TokenComment,
				Bytes: []byte("# freshness = " + freshnessComment + "\n"),
			}})
		}
	}
	for _, name := range unplacedAttrs(src, datasetAttrsFirst, datasetAttrsLast, datasetAttrsAfterStages) {
		copyAttr(body, src, name)
	}
	for _, name := range datasetAttrsLast {
		copyAttr(body, src, name)
	}

	// Stage blocks are appended whole. Their token stream is never rebuilt, which is what keeps a
	// pipeline heredoc byte-identical — with one deliberate exception: a pipeline line that is
	// only whitespace is normalized to truly empty before the block is copied. See
	// stripBlankPipelineLines for why a byte-for-byte copy is wrong for that one case.
	for _, block := range src.Blocks() {
		if block.Type() == "stage" {
			stripBlankPipelineLines(block)
		}
		body.AppendNewline()
		body.AppendBlock(block)
	}

	// Attributes placed after the stages — typically UI state blobs that are large and uninteresting
	// relative to the pipeline.
	for _, name := range datasetAttrsAfterStages {
		copyAttr(body, src, name)
	}
}

// stripBlankPipelineLines mutates a stage block's pipeline heredoc in place so any line that is
// only whitespace becomes truly empty.
//
// Terraform's flush heredoc (`<<-`) dedent explicitly excludes blank lines from the indentation it
// strips — a blank line's content passes through completely unmodified, however much whitespace it
// carries in the source. The Observe backend's own getTerraform rendering pads these separator
// lines with the same indentation as the surrounding pipeline content, purely for readability in
// the generated .tf text, but the live dataset's actual pipeline has no such padding. Copying the
// heredoc byte-for-byte therefore plans a change on every separator line: confirmed against a real
// `terraform plan` for eni_activity_test, which showed the state side genuinely empty and the
// config side carrying the padding. Normalizing whitespace-only lines to empty — and only those; a
// line with real content is never touched — is the one exception to copying pipeline tokens
// verbatim.
func stripBlankPipelineLines(stage *hclwrite.Block) {
	attr := stage.Body().GetAttribute("pipeline")
	if attr == nil {
		return
	}
	if stripped, changed := stripHeredocBlankLines(attr.Expr().BuildTokens(nil)); changed {
		stage.Body().SetAttributeRaw("pipeline", stripped)
	}
}

// stripHeredocBlankLines finds the first `<<-`/heredoc span in tokens and blanks out any interior
// TokenStringLit line that is whitespace-only, leaving every other token untouched.
//
// Heredoc content is tokenized one physical line per TokenStringLit, each ending in "\n", so a
// "blank" line is a token whose bytes are nothing but whitespace before that newline.
func stripHeredocBlankLines(tokens hclwrite.Tokens) (hclwrite.Tokens, bool) {
	start, end := -1, -1
	for i, t := range tokens {
		if t.Type == hclsyntax.TokenOHeredoc {
			start = i
		}
		if t.Type == hclsyntax.TokenCHeredoc {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		return tokens, false
	}

	changed := false
	out := make(hclwrite.Tokens, len(tokens))
	copy(out, tokens)
	for i := start + 1; i < end; i++ {
		t := out[i]
		if t.Type != hclsyntax.TokenStringLit {
			continue
		}
		line, hasNewline := strings.CutSuffix(string(t.Bytes), "\n")
		if !hasNewline || line == "" || strings.TrimSpace(line) != "" {
			continue // not a line token, already empty, or carries real content
		}
		out[i] = &hclwrite.Token{Type: hclsyntax.TokenStringLit, Bytes: []byte("\n"), SpacesBefore: t.SpacesBefore}
		changed = true
	}
	return out, changed
}

// writeCorrelationTag appends a tag resource to dst with its attributes in repo order.
func writeCorrelationTag(dst, src *hclwrite.Body, tagResource string) {
	body := dst.AppendNewBlock("resource", []string{"observe_correlation_tag", tagResource}).Body()
	for _, name := range correlationTagAttrs {
		copyAttr(body, src, name)
	}
	for _, name := range unplacedAttrs(src, correlationTagAttrs) {
		copyAttr(body, src, name)
	}
}

// writeResourceGrants appends an observe_resource_grants block for the dataset.
//
// The block's oid references the local dataset resource, and each grant block references the group
// via var.rbac_groups["<name>"].oid, matching the repo's convention.
func writeResourceGrants(dst *hclwrite.Body, resourceName string, grants []Grant) {
	body := dst.AppendNewBlock("resource", []string{"observe_resource_grants", resourceName}).Body()
	body.SetAttributeRaw("oid", hclwrite.TokensForTraversal(hcl.Traversal{
		hcl.TraverseRoot{Name: "observe_dataset"},
		hcl.TraverseAttr{Name: resourceName},
		hcl.TraverseAttr{Name: "oid"},
	}))
	for _, g := range grants {
		grantBlock := body.AppendNewBlock("grant", nil).Body()
		if g.GroupName != "" {
			// var.rbac_groups["NAME"].oid
			grantBlock.SetAttributeRaw("subject", hclwrite.TokensForTraversal(hcl.Traversal{
				hcl.TraverseRoot{Name: "var"},
				hcl.TraverseAttr{Name: "rbac_groups"},
				hcl.TraverseIndex{Key: cty.StringVal(g.GroupName)},
				hcl.TraverseAttr{Name: "oid"},
			}))
		} else {
			// Group not in var.rbac_groups. Emit the raw OID so the block is usable as-is,
			// with a comment pointing to the manual step.
			grantBlock.AppendUnstructuredTokens(hclwrite.Tokens{{
				Type:  hclsyntax.TokenComment,
				Bytes: []byte("# TODO: group " + g.RawSubject + " is not in var.rbac_groups — replace with var.rbac_groups[\"<name>\"].oid\n"),
			}})
			grantBlock.SetAttributeRaw("subject", hclwrite.TokensForValue(cty.StringVal(g.RawSubject)))
		}
		grantBlock.SetAttributeRaw("role", hclwrite.TokensForValue(cty.StringVal(g.Role)))
	}
}

// copyAttr moves one attribute across as a whole token stream, so a heredoc or a multi-line
// jsonencode survives byte for byte. An absent attribute is skipped.
func copyAttr(dst, src *hclwrite.Body, name string) {
	if attr := src.GetAttribute(name); attr != nil {
		dst.SetAttributeRaw(name, attr.Expr().BuildTokens(nil))
	}
}

// unplacedAttrs returns the attributes none of the given orders name, sorted.
//
// Sorting is not arbitrary: generated output is alphabetical, so this reproduces the order the
// provider emitted them in. Its real job is to give an attribute the repo has no convention for —
// including one the provider adds after this was written — a defined, stable position.
func unplacedAttrs(src *hclwrite.Body, placed ...[]string) []string {
	skip := map[string]bool{}
	for _, names := range placed {
		for _, name := range names {
			skip[name] = true
		}
	}
	var rest []string
	for name := range src.Attributes() {
		if !skip[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return rest
}

// shortDuration renders a Go duration literal the way the repo's freshness_overrides map does: the
// generator's "2m0s" becomes "2m" and "1h0m0s" becomes "1h", matching the 500-odd entries already
// there. Both spellings behave identically — the provider's freshness field uses
// diffSuppressTimeDuration, which parses each side before comparing.
func shortDuration(s string) string {
	d, err := time.ParseDuration(s)
	if err != nil || d == 0 {
		return s
	}
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

// splitResources picks the dataset block and any correlation tag blocks out of generated output, and
// names any other resource type it found.
//
// Reporting the remainder matters: an attribute the tool has never heard of is passed through, but a
// whole resource can only be dropped, so a future generator addition would otherwise vanish silently.
func splitResources(body *hclwrite.Body) (dataset *hclwrite.Block, tags []*hclwrite.Block, other []string) {
	for _, block := range body.Blocks() {
		labels := block.Labels()
		if block.Type() != "resource" || len(labels) != 2 {
			continue
		}
		switch labels[0] {
		case "observe_dataset":
			dataset = block
		case "observe_correlation_tag":
			tags = append(tags, block)
		default:
			other = append(other, labels[0]+"."+labels[1])
		}
	}
	return dataset, tags, other
}

func hasBlock(body *hclwrite.Body, blockType string) bool {
	for _, block := range body.Blocks() {
		if block.Type() == blockType {
			return true
		}
	}
	return false
}

// setAttr writes an attribute from expression source text.
func setAttr(body *hclwrite.Body, name, expr string) error {
	tokens, err := tokensForExpr(expr)
	if err != nil {
		return fmt.Errorf("attribute %s: %w", name, err)
	}
	body.SetAttributeRaw(name, tokens)
	return nil
}

// literalString reports whether source text is a plain string literal, and its value.
func literalString(src string) (string, bool) {
	expr, diags := hclsyntax.ParseExpression([]byte(src), "", hcl.InitialPos)
	if diags.HasErrors() {
		return "", false
	}
	v, diags := expr.Value(nil)
	if diags.HasErrors() || v.IsNull() || v.Type() != cty.String {
		return "", false
	}
	return v.AsString(), true
}
