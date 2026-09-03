package tf

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// moduleOutputRefs resolves oids belonging to a *different* module in the same repo.
//
// `piedpiper = module.piedpiper` binds that module's outputs object, so its datasets are reachable as
// `var.piedpiper.<output>.oid`. Nothing in the target module's own state describes them, and the general
// binding resolver only looks for data sources, so such an oid resolved at best to a dashboard-scoped
// data source and at worst not at all.
//
// The output names are read from the module's own `output` blocks rather than assumed to match resource
// names. They usually do, but the reference has to name the output that actually exists.
func (b *Builder) moduleOutputRefs() []Ref {
	var refs []Ref
	for varName, expr := range b.Repo.Bindings {
		moduleName, ok := parseModuleBinding(expr)
		if !ok {
			continue
		}
		source, declared := b.Repo.ModuleSources[moduleName]
		if !declared {
			continue
		}
		oids := b.Index.OIDsByModule["module."+moduleName]
		if len(oids) == 0 {
			continue
		}
		dir := filepath.Join(b.Repo.Root, filepath.FromSlash(strings.TrimPrefix(source, "./")))
		for outputName, resourceAddress := range moduleOutputs(dir) {
			oid, found := oids[resourceAddress]
			if !found {
				continue
			}
			refs = append(refs, Ref{
				OID:       oid,
				Reference: "var." + varName + "." + outputName + ".oid",
				Tier:      TierModuleVar,
			})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].OID != refs[j].OID {
			return refs[i].OID < refs[j].OID
		}
		return refs[i].Reference < refs[j].Reference
	})
	return refs
}

// parseModuleBinding matches a binding onto a whole module object, `module.<name>`.
//
// Only the bare form. `module.x.y` binds one output rather than the object, so `var.<binding>.<output>`
// would be wrong for it, and there is no evidence in the reference repo of that form being used for a
// dataset.
func parseModuleBinding(src string) (string, bool) {
	expr, diags := hclsyntax.ParseExpression([]byte(src), "binding", hcl.InitialPos)
	if diags.HasErrors() {
		return "", false
	}
	scope, isScope := expr.(*hclsyntax.ScopeTraversalExpr)
	if !isScope {
		return "", false
	}
	names := traversalNames(scope.Traversal)
	if len(names) != 2 || names[0] != "module" {
		return "", false
	}
	return names[1], true
}

// moduleOutputs reads a module directory's `output` blocks, returning output name -> the module-relative
// resource address its value refers to.
//
// Only an output whose value is a bare reference to a resource is useful here: `var.x.<output>.oid` reads
// an oid off the object, which needs the output to *be* that object. An output of `<resource>.oid`, or of
// anything computed, is skipped rather than guessed at.
func moduleOutputs(dir string) map[string]string {
	files, err := terraformFiles(dir)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		f, diags := hclsyntax.ParseConfig(src, file, hcl.InitialPos)
		if diags.HasErrors() {
			continue
		}
		body, isBody := f.Body.(*hclsyntax.Body)
		if !isBody {
			continue
		}
		for _, block := range body.Blocks {
			if block.Type != "output" || len(block.Labels) != 1 {
				continue
			}
			attr := block.Body.Attributes["value"]
			if attr == nil {
				continue
			}
			scope, isScope := attr.Expr.(*hclsyntax.ScopeTraversalExpr)
			if !isScope {
				continue
			}
			// `observe_dataset.cloudwatch_logs` — two steps, the whole resource object. Three would be
			// an attribute of it, which is not an object an oid can be read from.
			names := traversalNames(scope.Traversal)
			if len(names) != 2 {
				continue
			}
			out[block.Labels[0]] = strings.Join(names, ".")
		}
	}
	return out
}
