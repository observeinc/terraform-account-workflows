package tf

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// Tier records how an oid was resolved. Lower tiers are preferred.
type Tier int

const (
	// TierPeer is a managed dataset already in the target module.
	TierPeer Tier = iota
	// TierSelf is one of the datasets being imported in this run.
	TierSelf
	// TierWorkspace is the module's workspace variable. Ranked above TierModuleVar so the
	// workspace is labeled as itself: the root config also binds it as an ordinary module
	// variable, which resolves to the same reference but reads as noise in a report.
	TierWorkspace
	// TierModuleVar is a module input variable bound in the root config.
	TierModuleVar
	// TierModuleOID is a non-dataset data source in the target module, such as the
	// `observe_oid` block a storage integration is named through. These are hand-written for
	// exactly this purpose, so they need no review.
	TierModuleOID
	// TierModuleData is a dataset data source in the target module. Functional, but these are
	// mostly auto-generated as dashboard and monitor dependencies and named after the object
	// that pulled the dataset in, which reads poorly as a dataset input — so a reference here
	// usually wants promoting to a module variable.
	TierModuleData
	// TierUnresolved means no reference exists; the raw oid is left in place.
	TierUnresolved
)

func (t Tier) String() string {
	switch t {
	case TierPeer:
		return "module peer"
	case TierSelf:
		return "imported in this run"
	case TierModuleVar:
		return "module variable"
	case TierWorkspace:
		return "workspace variable"
	case TierModuleOID:
		return "in-module oid data source"
	case TierModuleData:
		return "in-module dataset data source"
	default:
		return "unresolved"
	}
}

// NeedsReview reports whether a resolution should block a clean exit.
func (t Tier) NeedsReview() bool { return t == TierModuleData || t == TierUnresolved }

// Ref is one resolved oid.
type Ref struct {
	OID       string
	Reference string
	Tier      Tier
}

// Map resolves oids to HCL reference expressions.
type Map struct {
	entries map[string]Ref
}

// Builder assembles a Map from the state index, the repo's module bindings, and the resource names
// assigned to the datasets being imported.
type Builder struct {
	Index    *Index
	Repo     *Repo
	SelfOIDs map[string]string // normalized dataset oid -> assigned terraform resource name
}

// Build resolves every reference source into a single lookup table.
func (b *Builder) Build() *Map {
	m := &Map{entries: map[string]Ref{}}

	// Highest tier last: put returns early if a better tier already claimed the oid.
	for oid, name := range b.SelfOIDs {
		m.put(Ref{OID: oid, Reference: fmt.Sprintf("observe_dataset.%s.oid", name), Tier: TierSelf})
	}
	for oid, entry := range b.Index.ManagedInModule {
		if entry.Type != "observe_dataset" {
			continue
		}
		m.put(Ref{OID: oid, Reference: entry.Address + ".oid", Tier: TierPeer})
	}
	if ws := b.Index.WorkspaceOID; ws != "" {
		m.put(Ref{OID: ws, Reference: "var.workspace.oid", Tier: TierWorkspace})
	}
	for _, ref := range b.moduleVarRefs() {
		m.put(ref)
	}
	// After the plain bindings: same tier, and a variable bound straight at a data source is a clearer
	// reference than one reached by decoding a document, so put's first-writer-wins keeps it.
	for _, ref := range b.appOutputRefs() {
		m.put(ref)
	}
	for _, ref := range b.moduleOutputRefs() {
		m.put(ref)
	}
	for oid, entry := range b.Index.DataInModule {
		tier := TierModuleOID
		if entry.Type == "observe_dataset" {
			tier = TierModuleData
		}
		m.put(Ref{OID: oid, Reference: entry.Address + ".oid", Tier: tier})
	}

	return m
}

// moduleVarRefs joins each module variable binding to the oid its target exposes, so a hardcoded
// oid becomes `var.<binding>`, `var.<binding>.dataset` or `var.<binding>.oid`.
//
// Which accessor is correct depends on what the binding traversed. `var.x = data.observe_datastream.a`
// binds the whole object, so `var.x.dataset` reads its dataset oid; `var.x = data.observe_datastream.a.dataset`
// binds a bare string, and the correct reference is `var.x` with no accessor at all. Emitting the
// wrong one fails at plan with "Unsupported attribute".
func (b *Builder) moduleVarRefs() []Ref {
	var refs []Ref
	for varName, expr := range b.Repo.Bindings {
		bindings := dataSourceBindingsIn(expr)
		if len(bindings) != 1 {
			// Zero means the binding is not a data source at all (`module.piedpiper`, a
			// `merge(...)` of RBAC groups). More than one means a conditional between two
			// different data sources, where which one applies depends on the workspace, so
			// there is no single correct reference.
			continue
		}
		target, ok := b.Index.RootByAddress[bindings[0].Address]
		if !ok {
			continue
		}

		switch bindings[0].Attribute {
		case "":
			// The whole object is bound, so both accessors are available on it. A datastream's
			// `dataset` is what a dataset input points at; the object's own `oid` is what other
			// attributes point at.
			if target.DatasetOID != "" {
				refs = append(refs, Ref{
					OID:       target.DatasetOID,
					Reference: fmt.Sprintf("var.%s.dataset", varName),
					Tier:      TierModuleVar,
				})
			}
			if target.OID != "" {
				refs = append(refs, Ref{
					OID:       target.OID,
					Reference: fmt.Sprintf("var.%s.oid", varName),
					Tier:      TierModuleVar,
				})
			}
		case "dataset":
			if target.DatasetOID != "" {
				refs = append(refs, Ref{OID: target.DatasetOID, Reference: "var." + varName, Tier: TierModuleVar})
			}
		case "oid":
			if target.OID != "" {
				refs = append(refs, Ref{OID: target.OID, Reference: "var." + varName, Tier: TierModuleVar})
			}
			// Any other attribute holds something that is not an oid, so there is nothing to map.
		}
	}
	// Bindings come from a map, so sort for a deterministic winner when two variables are bound
	// to the same object.
	sort.Slice(refs, func(i, j int) bool { return refs[i].Reference < refs[j].Reference })
	return refs
}

// dataSourceBinding is a `data.<type>.<name>` address a module variable is bound to, plus the single
// attribute the binding traversed past it, if any.
type dataSourceBinding struct {
	Address   string
	Attribute string
}

// dataSourceBindingsIn returns the data source bindings an expression resolves to.
//
// Recursion is deliberately narrow: only through conditionals, indexing, parentheses and template
// wrapping, which is what a count-gated binding looks like
// (`local.in_west ? null : data.observe_dataset.x[0]`). It does not descend into function calls,
// because a wrapped data source does not give the variable that data source's type —
// `jsondecode(data.observe_app.aws.outputs)` binds decoded JSON, and emitting `var.aws.oid` for it
// would be wrong.
func dataSourceBindingsIn(src string) []dataSourceBinding {
	expr, diags := hclsyntax.ParseExpression([]byte(src), "binding", hcl.InitialPos)
	if diags.HasErrors() {
		return nil
	}
	var found []dataSourceBinding
	collectDataSourceBindings(expr, &found)

	sort.Slice(found, func(i, j int) bool {
		if found[i].Address != found[j].Address {
			return found[i].Address < found[j].Address
		}
		return found[i].Attribute < found[j].Attribute
	})
	var unique []dataSourceBinding
	for i, b := range found {
		if i == 0 || b != found[i-1] {
			unique = append(unique, b)
		}
	}
	return unique
}

func collectDataSourceBindings(expr hclsyntax.Expression, out *[]dataSourceBinding) {
	switch e := expr.(type) {
	case *hclsyntax.ScopeTraversalExpr:
		if b, ok := dataSourceBindingOf(e.Traversal); ok {
			*out = append(*out, b)
		}
	case *hclsyntax.RelativeTraversalExpr:
		collectDataSourceBindings(e.Source, out)
	case *hclsyntax.IndexExpr:
		collectDataSourceBindings(e.Collection, out)
	case *hclsyntax.ConditionalExpr:
		collectDataSourceBindings(e.TrueResult, out)
		collectDataSourceBindings(e.FalseResult, out)
	case *hclsyntax.ParenthesesExpr:
		collectDataSourceBindings(e.Expression, out)
	case *hclsyntax.TemplateWrapExpr:
		// `"${data.observe_datastream.a.dataset}"` — a bare interpolation, same as the expression.
		collectDataSourceBindings(e.Wrapped, out)
	}
}

// dataSourceBindingOf splits a traversal into its leading `data.<type>.<name>` and the one attribute
// step that follows, if any.
//
// A single index directly after the address is allowed and ignored: that is a count-gated data source
// (`data.observe_dataset.x[0]`), which still yields the object. An index anywhere later is indexing
// *into* a value — `data.observe_app.aws.outputs["ds"]` — which is not the object or one of its oids,
// so it is refused rather than truncated. Truncating is what made the tool emit `var.x.dataset` for a
// variable already bound to a bare oid string.
func dataSourceBindingOf(traversal hcl.Traversal) (dataSourceBinding, bool) {
	var parts []string
	indexed := false
	for _, step := range traversal {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			parts = append(parts, s.Name)
		case hcl.TraverseAttr:
			if indexed && len(parts) > 3 {
				return dataSourceBinding{}, false
			}
			parts = append(parts, s.Name)
		case hcl.TraverseIndex:
			// Only the count gate, immediately after `data.<type>.<name>`.
			if len(parts) != 3 || indexed {
				return dataSourceBinding{}, false
			}
			indexed = true
		default:
			return dataSourceBinding{}, false
		}
	}
	if len(parts) < 3 || len(parts) > 4 || parts[0] != "data" {
		return dataSourceBinding{}, false
	}
	b := dataSourceBinding{Address: strings.Join(parts[:3], ".")}
	if len(parts) == 4 {
		b.Attribute = parts[3]
	}
	return b, true
}

// put records a reference unless an equal or better tier already claimed the oid.
func (m *Map) put(r Ref) {
	if r.OID == "" {
		return
	}
	if existing, ok := m.entries[r.OID]; ok && existing.Tier <= r.Tier {
		return
	}
	m.entries[r.OID] = r
}

// Resolve looks up an oid, normalizing away any version suffix first. An unknown oid comes back
// as TierUnresolved with an empty Reference.
func (m *Map) Resolve(oid string) Ref {
	normalized := NormalizeOID(oid)
	if r, ok := m.entries[normalized]; ok {
		return r
	}
	return Ref{OID: normalized, Tier: TierUnresolved}
}

// Len is the number of resolvable oids.
func (m *Map) Len() int { return len(m.entries) }
