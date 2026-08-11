package tf

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// findModuleBlock locates the root-module `module` block whose source points at moduleDir and
// reads its variable bindings.
//
// The repo has more than one module block (`example`, `piedpiper`), so matching on `source` rather
// than on block name is what keeps the two apart.
func findModuleBlock(root, moduleDir string) (path string, bindings map[string]string, hasOverrides bool, sources map[string]string, err error) {
	files, err := filepath.Glob(filepath.Join(root, "*.tf"))
	if err != nil {
		return "", nil, false, nil, err
	}
	sort.Strings(files)

	// Every module call is recorded, not just the target one: a module variable can be bound to another
	// module's outputs, and resolving that needs to know where that module's source lives.
	sources = map[string]string{}
	var target string
	want := "./" + filepath.ToSlash(moduleDir)
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			return "", nil, false, nil, err
		}
		f, diags := hclwrite.ParseConfig(src, file, hcl.InitialPos)
		if diags.HasErrors() {
			return "", nil, false, nil, fmt.Errorf("parse %s: %s", file, diags.Error())
		}
		for _, block := range f.Body().Blocks() {
			if block.Type() != "module" {
				continue
			}
			source := attrString(block.Body().GetAttribute("source"))
			if labels := block.Labels(); len(labels) == 1 && source != "" {
				sources[labels[0]] = source
			}
			if source != want && source != strings.TrimPrefix(want, "./") {
				continue
			}
			b := map[string]string{}
			for name, attr := range block.Body().Attributes() {
				if name == "source" {
					continue
				}
				if expr := strings.TrimSpace(string(attr.Expr().BuildTokens(nil).Bytes())); expr != "" {
					b[name] = expr
				}
			}
			_, hasOverrides = b["freshness_overrides"]
			target = file
			bindings = b
		}
	}
	if target == "" {
		return "", nil, false, nil, fmt.Errorf("no module block with source %q found in %s/*.tf", want, root)
	}
	return target, bindings, hasOverrides, sources, nil
}

// attrString reads a quoted string literal attribute, returning "" for anything else.
func attrString(attr *hclwrite.Attribute) string {
	if attr == nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(string(attr.Expr().BuildTokens(nil).Bytes())), `"`)
}

// FreshnessOverride is one entry to add or update in the root module block's freshness_overrides map.
type FreshnessOverride struct {
	Key   string
	Value string
}

// AddFreshnessOverrides upserts entries into the module block's freshness_overrides map: a key not
// yet present is added, and a key already present whose value differs is updated in place. A key
// already present with the same value is left untouched, so re-running with unchanged inputs produces
// byte-identical output. It returns the updated file bytes and the keys that were actually added or
// changed — the empty case (nothing to do) returns src unchanged and a nil list, not an error.
//
// Per-dataset freshness values live in this top-level map rather than in the module's own
// `local.freshness` base map, which holds a single entry.
//
// This edits raw bytes because hclwrite has no API for editing inside an object-cons expression. Both
// the insertion point for a new key and the replacement span for an updated one come from the parser's
// own source ranges, never from searching the file for matching text: two module blocks can carry
// byte-identical maps, and an empty `{}` matches almost anything.
//
// Updating a value here means a value someone hand-tuned for a dataset this run touches gets
// overwritten to match Observe's current value. That is intentional: this map is per-dataset, and any
// dataset this function is asked about is one the caller has decided Observe is the source of truth
// for on this run.
func AddFreshnessOverrides(src []byte, filename, moduleDir string, entries []FreshnessOverride) ([]byte, []string, error) {
	if len(entries) == 0 {
		return src, nil, nil
	}

	f, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("parse %s: %s", filename, diags.Error())
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil, nil, fmt.Errorf("parse %s: unexpected body type", filename)
	}

	want := "./" + filepath.ToSlash(moduleDir)
	for _, block := range body.Blocks {
		if block.Type != "module" {
			continue
		}
		if source := literalString(block.Body.Attributes["source"]); source != want && source != strings.TrimPrefix(want, "./") {
			continue
		}

		attr := block.Body.Attributes["freshness_overrides"]
		if attr == nil {
			return nil, nil, fmt.Errorf("module block in %s has no freshness_overrides map", filename)
		}
		object, ok := attr.Expr.(*hclsyntax.ObjectConsExpr)
		if !ok {
			return nil, nil, fmt.Errorf("freshness_overrides in %s is not a map literal", filename)
		}

		existing := existingValues(object)

		type valueEdit struct {
			start, end  int
			replacement string
		}
		var edits []valueEdit
		var added []FreshnessOverride
		var changed []string
		for _, e := range entries {
			cur, ok := existing[e.Key]
			switch {
			case !ok:
				added = append(added, e)
				changed = append(changed, fmt.Sprintf("%s: added (%s)", e.Key, e.Value))
			case cur.value != e.Value:
				edits = append(edits, valueEdit{cur.start, cur.end, fmt.Sprintf("%q", e.Value)})
				changed = append(changed, fmt.Sprintf("%s: %s -> %s", e.Key, cur.value, e.Value))
			}
			// Present with the same value: nothing to do.
		}
		if len(added) == 0 && len(edits) == 0 {
			return src, nil, nil
		}
		sort.Strings(changed)

		// Value edits are applied first. Each replaces exactly one item's value token, and items sit
		// strictly after the map's opening brace, so this never touches anything at or before it.
		sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
		out := make([]byte, 0, len(src))
		last := 0
		for _, ed := range edits {
			out = append(out, src[last:ed.start]...)
			out = append(out, ed.replacement...)
			last = ed.end
		}
		out = append(out, src[last:]...)

		if len(added) == 0 {
			return out, changed, nil
		}

		// The expression range starts at the `{`, so inserting just after it puts new entries at the
		// top of the map: one diff hunk, every existing entry byte-identical. `terraform fmt` aligns
		// the block afterwards. Its position in `out` is unchanged from `src`, since every value edit
		// above sits strictly after it.
		brace := attr.Expr.Range().Start.Byte
		if brace >= len(out) || out[brace] != '{' {
			return nil, nil, fmt.Errorf("freshness_overrides in %s does not start with '{'", filename)
		}

		var b strings.Builder
		for _, e := range added {
			fmt.Fprintf(&b, "\n    %q = %q,", e.Key, e.Value)
		}

		final := make([]byte, 0, len(out)+b.Len())
		final = append(final, out[:brace+1]...)
		final = append(final, b.String()...)
		final = append(final, out[brace+1:]...)
		return final, changed, nil
	}
	return nil, nil, fmt.Errorf("no module block with source %q found in %s", want, filename)
}

// existingValue is one map item's current value and the byte range that value occupies in the source,
// for an in-place replacement.
type existingValue struct {
	start, end int
	value      string
}

// existingValues reads the string keys and values already present in a map literal, with each value's
// source range. A computed key or value is skipped rather than guessed at, so it is treated as absent.
func existingValues(object *hclsyntax.ObjectConsExpr) map[string]existingValue {
	out := map[string]existingValue{}
	for _, item := range object.Items {
		k, kdiags := item.KeyExpr.Value(nil)
		if kdiags.HasErrors() || k.IsNull() || k.Type() != cty.String {
			continue
		}
		v, vdiags := item.ValueExpr.Value(nil)
		if vdiags.HasErrors() || v.IsNull() || v.Type() != cty.String {
			continue
		}
		rng := item.ValueExpr.Range()
		out[k.AsString()] = existingValue{start: rng.Start.Byte, end: rng.End.Byte, value: v.AsString()}
	}
	return out
}

// literalString reads a string-literal attribute, returning "" for anything else.
func literalString(attr *hclsyntax.Attribute) string {
	if attr == nil {
		return ""
	}
	v, diags := attr.Expr.Value(nil)
	if diags.HasErrors() || v.IsNull() || v.Type() != cty.String {
		return ""
	}
	return v.AsString()
}
