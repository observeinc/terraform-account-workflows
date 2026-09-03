package rewrite

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"tfdatasetimport/observe"
	"tfdatasetimport/tf"
)

// sanitizeIdentifier mirrors the terraform resolver's identifier rule: lowercase the last
// `/`-separated segment of the dataset name and replace anything not valid in an identifier.
//
// Reimplemented rather than imported because the resolver lives in the monorepo's bazel module.
// Reproducing it here is what lets the tool detect a collision before fetching, and gives the
// full-path fallback something to compare against.
func sanitizeIdentifier(name string) string {
	segments := strings.Split(name, "/")
	return sanitize(strings.ToLower(segments[len(segments)-1]))
}

// sanitizeFullPath keeps every segment, so `App Logs/Card Authorization` becomes
// `app_logs_card_authorization`. Suggested in the collision error because it collides on none of
// the repo's existing datasets.
func sanitizeFullPath(name string) string {
	return sanitize(strings.ToLower(name))
}

// These are copied verbatim from the resolver (`terraform/resolver/helpers.go`) and must stay that
// way: a hyphen is *valid* in a terraform identifier and is kept, and a run of invalid characters
// collapses to a single `_`. Diverging on either would make two distinct names sanitize to the same
// identifier — `trace.id` and `trace-id` both becoming `trace_id` is a duplicate resource, which
// terraform rejects for the whole module.
var (
	invalidIdentChars = regexp.MustCompile(`([^0-9a-zA-Z-_]+)`)
	leadingDigit      = regexp.MustCompile(`^[0-9]`)
)

func sanitize(s string) string {
	out := invalidIdentChars.ReplaceAllString(s, "_")
	if leadingDigit.MatchString(out) {
		out = "_" + out
	}
	if out == "" {
		return "_"
	}
	return out
}

// NameCollision describes one resource name that cannot be used.
type NameCollision struct {
	DatasetID   string
	DatasetName string
	Name        string
	// ConflictsWith is the existing address, or the other dataset id when two of the requested
	// datasets want the same name.
	ConflictsWith string
	// Suggestion is a name known to be free, offered for the -name-overrides file. Empty when no
	// suggestion is safe to make.
	Suggestion string
	// Note explains an obstacle the bare "taken by" wording would misstate. Optional.
	Note string
}

func (c NameCollision) String() string {
	s := fmt.Sprintf("dataset %s (%q) wants resource name %q, taken by %s",
		c.DatasetID, c.DatasetName, c.Name, c.ConflictsWith)
	if c.Note != "" {
		s += " — " + c.Note
	}
	if c.Suggestion != "" {
		s += fmt.Sprintf(" — try %q", c.Suggestion)
	}
	return s
}

// AssignNames picks a terraform resource name for each dataset.
//
// The generator's importName is used as-is: it matches many existing datasets by the last path
// segment's sanitized form, and nothing reproduces the rest that were hand-named semantically.
// Rather than inventing a disambiguated name, a collision is returned for the operator to settle in
// -name-overrides, so no resource silently lands under a name nobody chose.
//
// The second return value marks datasets the module already manages, by id — whoever declared them,
// tool or human — keyed to a human-readable description of where and how the match was made. Those
// keep their existing resource name and are regenerated in place rather than assigned a fresh one:
// this is what makes a dataset id double as an update mechanism, not just a create-once one. It also
// makes a no-op re-run a real no-op — regenerating from an unchanged live definition reproduces the
// file byte for byte — and it deliberately does not distinguish "the tool wrote this" from "a human
// wrote this by hand": once a dataset is under this repo's management at all, re-running for its id is
// how its config and freshness stay in sync with Observe, regardless of how it first got here.
func AssignNames(defs map[string]*observe.TerraformDefinition, repo *tf.Repo, index *tf.Index, overrides map[string]string) (names map[string]string, alreadyManaged map[string]string, collisions []NameCollision) {
	ids := make([]string, 0, len(defs))
	for id := range defs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	names = make(map[string]string, len(ids))
	alreadyManaged = make(map[string]string, len(ids))
	claimed := make(map[string]string, len(ids)) // lowercased name -> dataset id

	for _, id := range ids {
		def := defs[id]
		datasetName := DatasetName(def)

		// Identity before name. "Does the module already manage this dataset" is the question that
		// decides whether this is a create or an update, and it has to be asked of the dataset, not of
		// the name this tool happens to derive: `App Logs/Example Panel` derives `example_panel` while
		// the resource managing it may be called `app_logs_example_panel`, so a name-keyed lookup
		// misses it and — worse — goes on to suggest a free name, which would add a second resource
		// for it.
		if owner, found := managedAlready(repo, index, id, datasetName); found {
			names[id] = owner.resourceName
			alreadyManaged[id] = owner.where
			claimed[strings.ToLower(owner.resourceName)] = id
			continue
		}

		name := def.ImportName
		override, overridden := overrides[id]
		if overridden && override != "" {
			name = override
		} else if name == "" {
			name = sanitizeIdentifier(datasetName)
		}

		// Two datasets claiming one name is always a mistake, override or not: whichever is written
		// second would overwrite the first's file and both would import to the same address.
		key := strings.ToLower(name)
		if other, taken := claimed[key]; taken {
			collisions = append(collisions, NameCollision{
				DatasetID: id, DatasetName: datasetName, Name: name,
				ConflictsWith: "dataset " + other + " in this run",
				Suggestion:    freeName(sanitizeFullPath(datasetName), repo, claimed),
			})
			continue
		}

		if existing, taken := repo.Existing[key]; taken {
			// An override is the operator's explicit choice, but it cannot override the fact that the
			// name is taken by something else — that would clobber a hand-written resource.
			c := NameCollision{
				DatasetID: id, DatasetName: datasetName, Name: name,
				ConflictsWith: fmt.Sprintf("existing %s in %s", existing.Address, filepath.Base(existing.File)),
				Suggestion:    freeName(sanitizeFullPath(datasetName), repo, claimed),
			}
			// Terraform namespaces resource names per type, and keeps data sources in a namespace of
			// their own, so it would accept observe_dataset.x beside observe_link.x or beside
			// data.observe_dataset.x. Saying only "taken" would imply an HCL conflict that does not
			// exist; the real obstacle is that this tool writes one <name>.tf per resource.
			if !existing.IsResource || existing.Type != "observe_dataset" {
				c.Note = fmt.Sprintf("terraform would permit observe_dataset.%s beside it, but this tool writes %s.tf, which already exists", name, name)
			}
			collisions = append(collisions, c)
			continue
		}

		names[id] = name
		claimed[key] = id
	}

	return names, alreadyManaged, collisions
}

// datasetOwner is the resource in the module that already manages a dataset.
type datasetOwner struct {
	// resourceName is its terraform name, which a regenerated file has to keep so the resource is
	// replaced rather than duplicated under a second name.
	resourceName string
	// where reads as "<address> in <file> (matched on …)", for the report.
	where string
}

// managedAlready reports the resource in the module that already manages this dataset, and how it was
// identified.
//
// State is asked first because a dataset id is an exact key and survives anything the resource is
// called or named. The config index is the fallback for a resource declared but never applied, and for
// a run given no useful state; it compares Observe dataset names, so it can only answer when the
// module's name_format is resolvable.
func managedAlready(repo *tf.Repo, index *tf.Index, datasetID, datasetName string) (datasetOwner, bool) {
	describe := func(declared tf.ExistingResource, how string) datasetOwner {
		return datasetOwner{
			resourceName: nameOf(declared.Address),
			where:        fmt.Sprintf("%s in %s (%s)", declared.Address, filepath.Base(declared.File), how),
		}
	}

	if index != nil {
		if entry, ok := index.ManagedInModule[tf.NormalizeOID("o:::dataset:"+datasetID)]; ok {
			// State names the resource; the config says which file it is in and whether it is ours.
			if declared, ok := repo.Existing[strings.ToLower(entry.Name)]; ok {
				return describe(declared, "matched on dataset id in state"), true
			}
			// In state but not in the config: it was removed from the .tf files without being removed
			// from state. Adopting it again would collide on import, so it still counts as managed.
			return datasetOwner{
				resourceName: entry.Name,
				where:        entry.Address + " (in state, but no longer declared in the module)",
			}, true
		}
	}
	if datasetName != "" {
		if declared, ok := repo.ManagesDataset(datasetName); ok {
			return describe(declared, "matched on dataset name"), true
		}
	}
	return datasetOwner{}, false
}

// nameOf takes the terraform name off an address, so `observe_dataset.foo` yields `foo`.
func nameOf(address string) string {
	if i := strings.LastIndexByte(address, '.'); i >= 0 {
		return address[i+1:]
	}
	return address
}

// freeName returns base, or base with a numeric suffix, whichever is not yet taken.
func freeName(base string, repo *tf.Repo, claimed map[string]string) string {
	candidate := base
	for i := 2; ; i++ {
		key := strings.ToLower(candidate)
		_, inRepo := repo.Existing[key]
		_, inRun := claimed[key]
		if !inRepo && !inRun {
			return candidate
		}
		candidate = fmt.Sprintf("%s_%d", base, i)
	}
}

// DatasetName reads the Observe dataset name out of a generated definition.
func DatasetName(def *observe.TerraformDefinition) string {
	f, diags := hclwrite.ParseConfig([]byte(def.Resource), "generated.tf", hcl.InitialPos)
	if diags.HasErrors() {
		return ""
	}
	dataset, _, _ := splitResources(f.Body())
	if dataset == nil {
		return ""
	}
	name, _ := attrString(dataset.Body(), "name")
	return name
}

// UnadoptableReason explains why a dataset cannot be expressed as an `observe_dataset` resource at all,
// or returns "" when it can.
//
// A dataset with no `inputs` is not a transform: it is either a reference table, whose rows are uploaded
// rather than derived, or a raw ingest target fed by a datastream. `inputs` is Required on the
// observe_dataset resource schema, so no amount of rewriting produces a valid resource — the right
// answer is `observe_reference_table`, `observe_source_dataset`, or a data source referring to it, and
// which of those it is a judgement call this tool should not make.
//
// Reported up front rather than as a per-dataset rewrite failure, because "generated output has no stage
// block" reads as a defect in the tool when it is a statement about the dataset.
func UnadoptableReason(def *observe.TerraformDefinition) string {
	f, diags := hclwrite.ParseConfig([]byte(def.Resource), "generated.tf", hcl.InitialPos)
	if diags.HasErrors() {
		return ""
	}
	dataset, _, _ := splitResources(f.Body())
	if dataset == nil {
		return ""
	}
	body := dataset.Body()
	if body.GetAttribute("inputs") != nil {
		return ""
	}
	if hasBlock(body, "stage") {
		// Inputs missing but stages present is a shape this tool has not seen; let the rewrite try and
		// fail loudly rather than declaring it unadoptable on a guess.
		return ""
	}
	// The icon the generator gives a table-shaped dataset is the only other hint available, and it is
	// not load-bearing, so the reason stays about what is actually missing.
	return "no inputs and no stages: not a transform, so it cannot be an observe_dataset " +
		"(inputs is Required); it is a reference table or an ingest target"
}
