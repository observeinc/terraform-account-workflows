package tf

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// ExistingResource is one resource or data source already declared in the module.
type ExistingResource struct {
	// Address is the original-case address, for error messages. A data source carries the `data.`
	// prefix, matching how state-derived addresses read.
	Address string
	// File is the path it is declared in.
	File string
	// Type is the terraform type, e.g. `observe_dataset`.
	Type string
	// IsResource distinguishes a managed resource from a data source. The type label alone does not:
	// `data.observe_dataset.x` and `observe_dataset.x` share a type, and terraform keeps them in
	// separate namespaces, so only one of them actually conflicts with a dataset this tool writes.
	IsResource bool
	// DatasetName is the Observe dataset name an `observe_dataset` resource declares, unwrapped from
	// the module's `format(var.name_format, "…")` call. Empty for anything else, or when the name is
	// not a plain literal.
	DatasetName string
}

// scanExistingResources collects every resource and data source declared in the module directory,
// keyed by lowercased terraform name so collisions are caught case-insensitively.
//
// Account repos can mix case (`My_Dataset` alongside `myother`), and terraform treats the two as
// distinct addresses, but a case-only difference between a generated and an existing name is a
// mistake, not a plan.
//
// The second return value records, by lowercased name, whether an `observe_resource_grants` block
// with that name exists anywhere in the module. It can't be folded into the main map: a dataset and
// its grants companion share the exact same name, and the main map keeps only the first block seen
// per name (by design — that is what makes it answer "is this filename taken" correctly). Grants
// existence has to survive that collapsing to answer a different question: whether a dataset update
// needs a new import for its grants specifically, independent of whether the dataset itself does.
func scanExistingResources(moduleDir string) (map[string]ExistingResource, map[string]bool, error) {
	files, err := terraformFiles(moduleDir)
	if err != nil {
		return nil, nil, err
	}

	found := map[string]ExistingResource{}
	grantsDeclared := map[string]bool{}
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			return nil, nil, err
		}
		f, diags := hclsyntax.ParseConfig(src, file, hcl.InitialPos)
		if diags.HasErrors() {
			return nil, nil, fmt.Errorf("parse %s: %s", file, diags.Error())
		}
		body, ok := f.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, block := range body.Blocks {
			if block.Type != "resource" && block.Type != "data" {
				continue
			}
			if len(block.Labels) != 2 {
				continue
			}
			if block.Type == "resource" && block.Labels[0] == "observe_resource_grants" {
				grantsDeclared[strings.ToLower(block.Labels[1])] = true
			}
			key := strings.ToLower(block.Labels[1])
			if _, exists := found[key]; exists {
				continue
			}
			address := block.Labels[0] + "." + block.Labels[1]
			if block.Type == "data" {
				address = "data." + address
			}
			entry := ExistingResource{
				Address:    address,
				File:       file,
				Type:       block.Labels[0],
				IsResource: block.Type == "resource",
			}
			if entry.IsResource && block.Labels[0] == "observe_dataset" {
				entry.DatasetName = declaredDatasetName(block)
			}
			found[key] = entry
		}
	}
	return found, grantsDeclared, nil
}

// declaredDatasetName reads the Observe dataset name out of a committed resource block, unwrapping the
// module's `format(var.name_format, "…")` idiom. It returns "" when the name is computed, since an
// unknown name must not be mistaken for a match.
func declaredDatasetName(block *hclsyntax.Block) string {
	attr := block.Body.Attributes["name"]
	if attr == nil {
		return ""
	}
	expr := attr.Expr
	// Unwrap format(var.name_format, "<name>") down to its last literal argument.
	if call, ok := expr.(*hclsyntax.FunctionCallExpr); ok {
		if call.Name != "format" || len(call.Args) == 0 {
			return ""
		}
		expr = call.Args[len(call.Args)-1]
	}
	v, diags := expr.Value(nil)
	if diags.HasErrors() || v.IsNull() || v.Type().FriendlyName() != "string" {
		return ""
	}
	return v.AsString()
}

// DeclaresOnlyDataset reports whether path declares nothing but the given `observe_dataset` resource,
// its correlation tags, and its `observe_resource_grants` companion — that is, whether it is safe to
// overwrite with a freshly generated version of the same dataset.
//
// This is defence in depth behind the resource-name check, which normally catches an already-declared
// dataset first. It exists because that check only sees files that *declare a resource*: a dataset
// whose generated name is `main` or `variables` would otherwise overwrite a module file that declares
// none.
//
// Anything else, including a hand-written file that happens to declare one dataset under a different
// name, comes back false.
func DeclaresOnlyDataset(path, resourceName string) (bool, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	f, diags := hclsyntax.ParseConfig(src, path, hcl.InitialPos)
	if diags.HasErrors() {
		// An unparseable file is not ours, and overwriting it silently would be worse than stopping.
		return false, nil
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return false, nil
	}
	return declaresOnlyDataset(body, resourceName), nil
}

// declaresOnlyDataset is the shared predicate behind the pre-write guard.
func declaresOnlyDataset(body *hclsyntax.Body, resourceName string) bool {
	foundDataset := false
	for _, block := range body.Blocks {
		switch {
		case block.Type == "resource" && len(block.Labels) == 2 && block.Labels[0] == "observe_dataset":
			if block.Labels[1] != resourceName {
				return false
			}
			foundDataset = true
		case block.Type == "resource" && len(block.Labels) == 2 && block.Labels[0] == "observe_correlation_tag":
			if !strings.HasPrefix(block.Labels[1], resourceName+"_") {
				return false
			}
		case block.Type == "resource" && len(block.Labels) == 2 && block.Labels[0] == "observe_resource_grants":
			// Grants share the dataset's exact name, not a suffixed one — there is only ever one
			// grants resource per dataset, so an exact match is the right test, unlike the tag prefix
			// check above.
			if block.Labels[1] != resourceName {
				return false
			}
		default:
			return false
		}
	}
	return foundDataset
}

// DeclaresOnlyImports reports whether path holds nothing but `import` blocks — that is, whether it is
// this tool's own earlier imports.tf and so safe to replace.
//
// A file with no import blocks at all comes back false, including an empty one: claiming an unrelated
// file because it happens to declare nothing is exactly the mistake this guards against.
func DeclaresOnlyImports(path string) (bool, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	f, diags := hclsyntax.ParseConfig(src, path, hcl.InitialPos)
	if diags.HasErrors() {
		return false, nil
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return false, nil
	}
	if len(body.Attributes) > 0 {
		return false, nil
	}
	for _, block := range body.Blocks {
		if block.Type != "import" {
			return false, nil
		}
	}
	return len(body.Blocks) > 0, nil
}

// terraformFiles lists the module's terraform sources. The extension is matched case-insensitively
// because the repo contains at least one `.TF` file.
func terraformFiles(dir string) ([]string, error) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range dirEntries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".tf") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}
