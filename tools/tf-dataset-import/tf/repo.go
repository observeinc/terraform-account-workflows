package tf

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Repo is a loaded terraform account repo.
type Repo struct {
	// Root is the absolute repo path.
	Root string
	// ModuleDir is the repo-relative module directory, e.g. `modules/example`.
	ModuleDir string
	// RootConfigPath is the root .tf file declaring the module block.
	RootConfigPath string
	// Bindings maps a module variable name to the root-module address it is bound to, read from
	// the module block, e.g. `applogs_datastream` -> `data.observe_datastream.applogs`.
	Bindings map[string]string
	// HasFreshnessOverrides records whether the module block passes a freshness_overrides map.
	HasFreshnessOverrides bool
	// Existing holds every resource and data source already declared in the module, keyed by
	// lowercased terraform name so collisions can be detected case-insensitively.
	Existing map[string]ExistingResource
	// ExistingDatasets holds only `observe_dataset` resources, keyed the same way as Existing. Use
	// this, not Existing, to find where a dataset resource itself is declared: Existing keeps only
	// the first block seen per name, and a dataset's grants companion shares its exact name, so
	// Existing[key] can resolve to the grants block instead when one precedes the other in scan
	// order. See scanExistingResources for the full reasoning.
	ExistingDatasets map[string]ExistingResource
	// ExistingGrants holds only `observe_resource_grants` resources, keyed the same way. An update
	// consults this to decide whether a dataset's grants block already exists (and where, to replace
	// it in place) or needs inserting fresh.
	ExistingGrants map[string]ExistingResource
	// GrantsDeclared records, by lowercased resource name, whether an observe_resource_grants block
	// with that name is already declared in the module. Derived from ExistingGrants; kept as its own
	// bool map because most callers only need the yes/no answer.
	GrantsDeclared map[string]bool
	// ModuleSources maps every module label called from the root config to its source directory, so a
	// variable bound to another module's outputs can be followed to that module's own declarations.
	ModuleSources map[string]string

	// managedDatasets maps an Observe dataset name to the module resource declaring it. This is the
	// index that answers "does the repo already manage this dataset", which is a different question
	// from "is this terraform name taken" — the repo hand-names most of its datasets, so the two
	// coincide only by chance.
	managedDatasets map[string]ExistingResource
	// nameFormatKnown records whether name_format resolved to a literal. When it did not, a declared
	// name cannot be compared against a live one and managedDatasets is left empty rather than
	// populated with values that would not match.
	nameFormatKnown bool
}

// ManagesDataset returns the module resource already managing the Observe dataset called
// datasetName, whatever terraform resource name it carries.
//
// Callers must treat a false result as "not found by name" rather than "not managed": it is also what
// comes back when name_format could not be resolved. NameFormatKnown distinguishes the two.
func (r *Repo) ManagesDataset(datasetName string) (ExistingResource, bool) {
	e, ok := r.managedDatasets[datasetName]
	return e, ok
}

// NameFormatKnown reports whether the module's name_format could be resolved, and so whether
// ManagesDataset is able to answer at all.
func (r *Repo) NameFormatKnown() bool { return r.nameFormatKnown }

// nameFormat is the format the module wraps dataset names in, from the module block's name_format
// binding.
//
// An unbound name_format means the module variable's own default applies. That default is `%s` in the
// reference repo, and this tool cannot read a module's variable defaults, so an absent binding is
// taken to mean the identity format — the assumption is stated here because a wrong one produces
// silent false negatives.
func (r *Repo) nameFormat() (string, bool) {
	raw, bound := r.Bindings["name_format"]
	if !bound {
		return "%s", true
	}
	format, err := strconv.Unquote(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	return format, true
}

// FreshnessDefault is the module's fallback freshness, read from the module block's
// `freshness_default` binding. Empty when the block does not set one, in which case the module's own
// variable default applies and this tool has no way to know it.
func (r *Repo) FreshnessDefault() string {
	return strings.Trim(strings.TrimSpace(r.Bindings["freshness_default"]), `"`)
}

// FreshnessOverrides decodes the module block's freshness_overrides map to key → value.
//
// Read from the binding text already captured at load time, so this costs no extra file access. A
// computed key or value is skipped rather than guessed at.
func (r *Repo) FreshnessOverrides() map[string]string {
	raw := strings.TrimSpace(r.Bindings["freshness_overrides"])
	if raw == "" {
		return nil
	}
	expr, diags := hclsyntax.ParseExpression([]byte(raw), "freshness_overrides", hcl.InitialPos)
	if diags.HasErrors() {
		return nil
	}
	object, ok := expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(object.Items))
	for _, item := range object.Items {
		k, kdiags := item.KeyExpr.Value(nil)
		v, vdiags := item.ValueExpr.Value(nil)
		if kdiags.HasErrors() || vdiags.HasErrors() || k.IsNull() || v.IsNull() {
			continue
		}
		if k.Type() != cty.String || v.Type() != cty.String {
			continue
		}
		out[k.AsString()] = v.AsString()
	}
	return out
}

// Load reads the repo's root config and module directory.
func LoadRepo(root, moduleDir string) (*Repo, error) {
	info, err := os.Stat(filepath.Join(root, moduleDir))
	if err != nil {
		return nil, fmt.Errorf("module dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("module dir %s is not a directory", moduleDir)
	}

	r := &Repo{Root: root, ModuleDir: moduleDir}

	path, bindings, hasOverrides, sources, err := findModuleBlock(root, moduleDir)
	if err != nil {
		return nil, err
	}
	r.RootConfigPath = path
	r.Bindings = bindings
	r.HasFreshnessOverrides = hasOverrides
	r.ModuleSources = sources

	existing, datasets, grants, err := scanExistingResources(filepath.Join(root, moduleDir))
	if err != nil {
		return nil, err
	}
	r.Existing = existing
	r.ExistingDatasets = datasets
	r.ExistingGrants = grants
	r.GrantsDeclared = make(map[string]bool, len(grants))
	for key := range grants {
		r.GrantsDeclared[key] = true
	}
	r.indexManagedDatasets()

	return r, nil
}

// indexManagedDatasets builds the dataset-name index, applying name_format so a declared literal can
// be compared against the live name a generated definition carries.
func (r *Repo) indexManagedDatasets() {
	r.managedDatasets = map[string]ExistingResource{}

	format, known := r.nameFormat()
	r.nameFormatKnown = known
	if !known {
		return
	}
	for _, e := range r.ExistingDatasets {
		// ExistingDatasets already guarantees a managed observe_dataset resource; only a computed
		// name (declaredDatasetName returning "") is left to skip here.
		if e.DatasetName == "" {
			continue
		}
		full := e.DatasetName
		if format != "%s" {
			full = fmt.Sprintf(format, e.DatasetName)
		}
		// First declaration wins, matching scanExistingResources. A duplicate dataset name is
		// already broken in the repo and not this tool's to adjudicate.
		if _, dup := r.managedDatasets[full]; !dup {
			r.managedDatasets[full] = e
		}
	}
}

// ModulePath is the absolute path to the module directory.
func (r *Repo) ModulePath() string { return filepath.Join(r.Root, r.ModuleDir) }
