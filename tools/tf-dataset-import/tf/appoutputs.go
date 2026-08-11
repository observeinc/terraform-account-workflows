package tf

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// appBinding is a module variable bound into an app's decoded outputs, e.g.
// `aws = jsondecode(data.observe_app.aws.outputs)` or `ec2 = jsondecode(...).ec2`.
type appBinding struct {
	varName string
	// address is the data source holding the JSON, e.g. `data.observe_app.aws`.
	address string
	// prefix is the path traversed after the decode, so `["ec2"]` for the second example. The oids this
	// variable can reach are the ones at or below it.
	prefix []string
}

// appOutputRefs resolves oids that only exist inside an app's `outputs` JSON.
//
// An installed app's datasets are not separate state resources: the app records them in one `outputs`
// attribute, a JSON document whose leaves carry an `oid`. The repo reaches them by decoding it —
// `aws = jsondecode(data.observe_app.aws.outputs)` — and referring to `var.aws.config_record.oid`.
//
// Nothing about the data source's own oid describes those datasets, and the general binding resolver
// deliberately refuses to look inside a function call, because a wrapped data source does not give the
// variable that data source's type. So this walks the decoded JSON instead, which is the only place the
// mapping exists.
func (b *Builder) appOutputRefs() []Ref {
	bindings := b.appBindings()
	if len(bindings) == 0 {
		return nil
	}

	// Decode each app's outputs once, however many variables are bound into it.
	decoded := map[string]map[string]any{}
	for _, binding := range bindings {
		if _, done := decoded[binding.address]; done {
			continue
		}
		target, ok := b.Index.RootByAddress[binding.address]
		if !ok || target.Outputs == "" {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(target.Outputs), &doc); err != nil {
			// Not a JSON object, so there is nothing to walk. Leaving the oids unresolved is
			// reported; guessing a reference would not be.
			continue
		}
		decoded[binding.address] = doc
	}

	// candidates keeps every reference that could name an oid, so the shortest can win.
	candidates := map[string][]string{}
	for _, binding := range bindings {
		doc, ok := decoded[binding.address]
		if !ok {
			continue
		}
		for path, oid := range oidsInJSON(doc) {
			rest, within := trimPrefix(strings.Split(path, "\x00"), binding.prefix)
			if !within {
				continue
			}
			parts := append([]string{"var." + binding.varName}, rest...)
			candidates[oid] = append(candidates[oid], strings.Join(parts, ".")+".oid")
		}
	}

	refs := make([]Ref, 0, len(candidates))
	for oid, options := range candidates {
		// Shortest wins, ties broken alphabetically for a deterministic answer. The repo writes
		// `var.ec2.instance.oid` rather than `var.aws.ec2.instance.oid`, and a variable bound deeper
		// into the document is the one whose name says what it holds.
		sort.Slice(options, func(i, j int) bool {
			if len(options[i]) != len(options[j]) {
				return len(options[i]) < len(options[j])
			}
			return options[i] < options[j]
		})
		refs = append(refs, Ref{OID: NormalizeOID(oid), Reference: options[0], Tier: TierModuleVar})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].OID < refs[j].OID })
	return refs
}

// appBindings finds the module variables bound into a decoded `outputs` document.
func (b *Builder) appBindings() []appBinding {
	var out []appBinding
	for varName, expr := range b.Repo.Bindings {
		address, prefix, ok := parseJSONDecodeBinding(expr)
		if !ok {
			continue
		}
		out = append(out, appBinding{varName: varName, address: address, prefix: prefix})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].varName < out[j].varName })
	return out
}

// parseJSONDecodeBinding matches `jsondecode(<address>.outputs)` with any number of trailing attribute
// steps, and returns the address plus those steps.
//
// Deliberately shape-specific. Anything else wrapped in a function call stays unresolved, because the
// point of refusing function calls generally is that the result's type is unknown; this one shape is
// understood well enough to walk.
func parseJSONDecodeBinding(src string) (address string, prefix []string, ok bool) {
	expr, diags := hclsyntax.ParseExpression([]byte(src), "binding", hcl.InitialPos)
	if diags.HasErrors() {
		return "", nil, false
	}

	// Trailing attribute access lands as a relative traversal over the call.
	if rel, isRel := expr.(*hclsyntax.RelativeTraversalExpr); isRel {
		for _, step := range rel.Traversal {
			attr, isAttr := step.(hcl.TraverseAttr)
			if !isAttr {
				return "", nil, false
			}
			prefix = append(prefix, attr.Name)
		}
		expr = rel.Source
	}

	call, isCall := expr.(*hclsyntax.FunctionCallExpr)
	if !isCall || call.Name != "jsondecode" || len(call.Args) != 1 {
		return "", nil, false
	}
	scope, isScope := call.Args[0].(*hclsyntax.ScopeTraversalExpr)
	if !isScope {
		return "", nil, false
	}

	names := traversalNames(scope.Traversal)
	// `data` `observe_app` `aws` `outputs` — the attribute read has to be `outputs`, since that is the
	// only one holding a document to walk.
	if len(names) < 3 || names[len(names)-1] != "outputs" {
		return "", nil, false
	}
	return strings.Join(names[:len(names)-1], "."), prefix, true
}

// traversalNames flattens a traversal to its plain name steps, giving up on anything else so an index
// or a splat cannot be silently read as an attribute.
func traversalNames(t hcl.Traversal) []string {
	out := make([]string, 0, len(t))
	for i, step := range t {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			if i != 0 {
				return nil
			}
			out = append(out, s.Name)
		case hcl.TraverseAttr:
			out = append(out, s.Name)
		default:
			return nil
		}
	}
	return out
}

// oidsInJSON walks a decoded document and returns every `oid` it finds, keyed by the NUL-joined path of
// the object holding it.
//
// An object carrying a string `oid` is treated as the thing that oid names, and its own children are
// still walked: an app groups datasets under a component key, and a nested one is reachable in its own
// right.
func oidsInJSON(doc map[string]any) map[string]string {
	found := map[string]string{}
	var walk func(node any, path []string)
	walk = func(node any, path []string) {
		object, isObject := node.(map[string]any)
		if !isObject {
			return
		}
		if oid, isString := object["oid"].(string); isString && IsOID(oid) && len(path) > 0 {
			found[strings.Join(path, "\x00")] = oid
		}
		for key, child := range object {
			if key == "oid" {
				continue
			}
			// A fresh slice per child: appending to the shared one would let a sibling overwrite the
			// step just written and mis-key every path below this point.
			next := make([]string, len(path), len(path)+1)
			copy(next, path)
			walk(child, append(next, key))
		}
	}
	walk(doc, nil)
	return found
}

// trimPrefix reports whether path sits at or below prefix, and returns what remains below it.
func trimPrefix(path, prefix []string) ([]string, bool) {
	if len(path) < len(prefix) {
		return nil, false
	}
	for i, want := range prefix {
		if path[i] != want {
			return nil, false
		}
	}
	rest := path[len(prefix):]
	if len(rest) == 0 {
		// The variable is bound directly to the object holding the oid, so `var.<name>.oid` reads it.
		return nil, true
	}
	return rest, true
}
