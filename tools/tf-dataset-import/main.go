// Command tf-dataset-import generates repo-conventional terraform for an Observe dataset and the
// config-driven import blocks that bring it into an account repo's state.
//
// A dataset id already under this repo's management is not skipped — it is treated as an update:
// its resource block is regenerated from the dataset's current live definition and spliced back into
// whatever file already declares it (never rewritten to a new file named after the resource), and
// its freshness_overrides entry is kept in sync, so re-running for the same id is how a dataset that
// changed in Observe stays in sync in Terraform, and re-running with nothing changed is a genuine
// no-op.
//
// The tool never talks to a terraform backend. It reads the Observe API and local files, and writes
// .tf files plus the import blocks; performing the import (`terraform apply`) is a deliberate,
// separate step.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"tfdatasetimport/observe"
	"tfdatasetimport/output"
	"tfdatasetimport/rewrite"
	"tfdatasetimport/tf"
)

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "tf-dataset-import: %v\n", err)
		os.Exit(2)
	}

	needsAttention, err := run(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tf-dataset-import: %v\n", err)
		os.Exit(1)
	}
	if needsAttention {
		// Non-zero so an unwired input or a name collision cannot slip past a scripted run.
		os.Exit(3)
	}
}

func run(ctx context.Context, cfg *Config) (bool, error) {
	report := output.NewReport()

	repo, err := tf.LoadRepo(cfg.Repo, cfg.ModuleDir)
	if err != nil {
		return false, err
	}
	fmt.Printf("repo      %s\n", cfg.Repo)
	fmt.Printf("module    %s  (%d existing resource names, %d module bindings)\n",
		cfg.ModuleDir, len(repo.Existing), len(repo.Bindings))

	// The lookup this tool emits falls back to var.freshness_default, so a per-dataset override is
	// only needed when the dataset differs from it. Guessing that default would silently change
	// materialization cadence, so an unreadable one is fatal rather than assumed.
	freshnessDefault := repo.FreshnessDefault()
	if freshnessDefault == "" {
		return false, fmt.Errorf("module block in %s does not set freshness_default; "+
			"cannot tell which datasets need a freshness_overrides entry", repo.RootConfigPath)
	}
	fmt.Printf("freshness %s default\n", freshnessDefault)

	state, err := tf.LoadState(cfg.StatePath)
	if err != nil {
		return false, err
	}
	index := tf.BuildIndex(state, cfg.ModuleAddress())
	graph := tf.BuildGraph(state)
	fmt.Printf("state     %s  (%d managed, %d data in %s)\n",
		filepath.Base(cfg.StatePath), len(index.ManagedInModule), len(index.DataInModule), cfg.ModuleAddress())

	defs, cache, err := fetchAll(ctx, cfg)
	if err != nil {
		return false, err
	}
	report.CacheHits, report.CacheMisses = cache.Hits, cache.Misses

	// Set aside what cannot become an observe_dataset before naming anything, so these do not consume a
	// resource name and do not surface later as a rewrite failure that reads like a tool defect.
	for _, id := range sortedIDs(defs) {
		if reason := rewrite.UnadoptableReason(defs[id]); reason != "" {
			report.Unadoptable = append(report.Unadoptable,
				fmt.Sprintf("%s (%q): %s", id, rewrite.DatasetName(defs[id]), reason))
			delete(defs, id)
		}
	}

	names, alreadyManaged, collisions := rewrite.AssignNames(defs, repo, index, cfg.NameOverrides)
	report.Updated = updatedReport(defs, alreadyManaged)
	// Without a resolvable name_format, an already-managed dataset can only be recognised from state.
	// Saying so matters: the consequence of missing one is a second resource for the same dataset.
	if !repo.NameFormatKnown() {
		fmt.Printf("warning   module block binds name_format to a non-literal; an already-managed\n")
		fmt.Printf("          dataset can only be detected from state, not from the .tf files\n")
	}
	if len(collisions) > 0 {
		for _, c := range collisions {
			report.NameCollisions = append(report.NameCollisions, c.String())
		}
		report.Write(os.Stdout)
		return true, nil
	}

	// A destination that exists but does not merely declare the dataset (and, for an update, its
	// grants companion) being written would be overwritten, so the run stops before writing anything.
	clobbers, err := destinationConflicts(cfg, repo, names, alreadyManaged)
	if err != nil {
		return false, err
	}
	if len(clobbers) > 0 {
		report.Clobbers = clobbers
		report.Write(os.Stdout)
		return true, nil
	}

	refs := buildRefMap(defs, names, index, repo)

	grants, grantsWithRawIDs, err := fetchGrants(ctx, cfg, names, index)
	if err != nil {
		return false, err
	}
	report.GrantsWithRawIDs = grantsWithRawIDs

	results, failures, err := rewriteAll(cfg, defs, names, grants, refs, freshnessDefault)
	if err != nil {
		return false, err
	}
	report.Failed = failures

	// Computed here, not inside writeAll, so a dry run previews it too.
	freshnessBytes, freshnessChanged, err := computeFreshnessOverrides(repo, cfg.ModuleDir, results)
	if err != nil {
		return false, err
	}
	report.FreshnessUpdated = freshnessChanged

	if cfg.TargetState != "" {
		// Only the newly-created subset: an already-managed dataset is by definition already applied
		// in every workspace the module reaches, so checking it again for a name collision is noise.
		conflicts, err := replicaConflicts(cfg.TargetState, newResultsOnly(results, alreadyManaged))
		if err != nil {
			return false, err
		}
		report.TargetStateConflicts = conflicts
	}

	// Resolved once, up front: an already-managed dataset's placement is the file (and byte range)
	// it is already declared in, never the <name>.tf convention, so both the manifest/report (which
	// need to say where a file lives) and writeAll (which needs to actually write there) have to
	// agree on the same answer.
	plans, err := planResults(repo, results)
	if err != nil {
		return false, err
	}

	manifest := &output.Manifest{
		CustomerID:    cfg.CustomerID,
		Domain:        cfg.Domain,
		ModuleDir:     cfg.ModuleDir,
		ModuleAddress: cfg.ModuleAddress(),
		TFWorkspace:   cfg.TFWorkspace,
	}
	for _, res := range results {
		address := fmt.Sprintf("%s.observe_dataset.%s", cfg.ModuleAddress(), res.ResourceName)
		entry := output.NewEntry(res, plans[res.DatasetID].RelFile, address)
		entry.HasGrants = len(grants[res.DatasetID]) > 0
		_, entry.AlreadyManaged = alreadyManaged[res.DatasetID]
		entry.GrantsAlreadyManaged = grantsAlreadyManaged(repo, index, res.ResourceName)
		manifest.Entries = append(manifest.Entries, entry)
		report.Add(entry)
	}
	manifest.BlastRadius = len(graph.TransitiveDependents(datasetOIDs(defs)))
	report.BlastRadius = manifest.BlastRadius
	report.EmittedCorrelationTags = cfg.EmitCorrelationTags

	if cfg.DryRun {
		fmt.Printf("\ndry run: would write %d files under %s\n", len(touchedFiles(plans)), cfg.ModuleDir)
		if n := output.PendingImportCount(manifest); n > 0 {
			fmt.Printf("dry run: would write %d import blocks to %s\n", n, filepath.Join(cfg.Repo, output.ImportBlocksFile))
		}
		report.Write(os.Stdout)
		return report.NeedsAttention(), nil
	}

	if err := writeAll(cfg, repo, results, plans, manifest, freshnessBytes, freshnessChanged); err != nil {
		return false, err
	}
	if err := report.WriteJSON(filepath.Join(cfg.OutDir, "report.json")); err != nil {
		return false, err
	}

	report.Write(os.Stdout)
	return report.NeedsAttention(), nil
}

// updatedReport renders alreadyManaged (dataset id -> where it is already managed) as sorted,
// human-readable lines for the report.
func updatedReport(defs map[string]*observe.TerraformDefinition, alreadyManaged map[string]string) []string {
	out := make([]string, 0, len(alreadyManaged))
	for id, where := range alreadyManaged {
		out = append(out, fmt.Sprintf("%s (%q) already managed as %s", id, rewrite.DatasetName(defs[id]), where))
	}
	sort.Strings(out)
	return out
}

// newResultsOnly filters to the results whose dataset was not already managed before this run.
func newResultsOnly(results []*rewrite.Result, alreadyManaged map[string]string) []*rewrite.Result {
	out := make([]*rewrite.Result, 0, len(results))
	for _, res := range results {
		if _, already := alreadyManaged[res.DatasetID]; !already {
			out = append(out, res)
		}
	}
	return out
}

// grantsAlreadyManaged reports whether an observe_resource_grants resource under this name is already
// declared in the module or already in state — the two places a grants import already exists means
// nothing new needs writing. See tf.Index.GrantsManagedInModule and tf.Repo.GrantsDeclared for why
// these can't be answered from the identity checks that already exist for the dataset itself.
func grantsAlreadyManaged(repo *tf.Repo, index *tf.Index, resourceName string) bool {
	key := strings.ToLower(resourceName)
	return repo.GrantsDeclared[key] || index.GrantsManagedInModule[key]
}

// sortedIDs gives a deterministic iteration order over the fetched definitions.
func sortedIDs(defs map[string]*observe.TerraformDefinition) []string {
	ids := make([]string, 0, len(defs))
	for id := range defs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// fetchAll retrieves generated terraform for every requested dataset, through the disk cache.
//
// Calls are sequential: generation takes about half a second per dataset, so a hundred is about a
// minute, and a single-threaded loop keeps failures easy to attribute.
func fetchAll(ctx context.Context, cfg *Config) (map[string]*observe.TerraformDefinition, *observe.Cache, error) {
	client := observe.New(cfg.MetaURL(), cfg.CustomerID, cfg.Token)
	cache := observe.NewCache(cfg.CacheDir, cfg.CustomerID, client)

	defs := make(map[string]*observe.TerraformDefinition, len(cfg.DatasetIDs))
	for i, id := range cfg.DatasetIDs {
		def, err := cache.GetDatasetTerraform(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		defs[id] = def
		fmt.Printf("\rfetch     %d/%d datasets", i+1, len(cfg.DatasetIDs))
	}
	fmt.Println()
	return defs, cache, nil
}

// fetchGrants fetches RBAC grants for each dataset being adopted.
//
// Only group grants are returned; user grants are dropped. Each group ID is resolved to a name via
// the state's observe_rbac_group index. Resolved groups become var.rbac_groups["NAME"].oid references;
// unresolved groups are emitted as raw OID string literals with a TODO comment so the block is
// immediately usable and the operator knows which subjects still need wiring.
func fetchGrants(ctx context.Context, cfg *Config, names map[string]string, index *tf.Index) (map[string][]rewrite.Grant, []string, error) {
	if len(index.GroupNames) == 0 {
		// No groups in state means the repo has no rbac_groups variable, so grants cannot be wired.
		// This is fine for repos that do not manage RBAC — return an empty map.
		return map[string][]rewrite.Grant{}, nil, nil
	}

	client := observe.New(cfg.MetaURL(), cfg.CustomerID, cfg.Token)
	out := make(map[string][]rewrite.Grant, len(names))
	var rawIDEntries []string

	ids := make([]string, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for i, id := range ids {
		raw, err := client.GetDatasetGrants(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		var grants []rewrite.Grant
		var missing []string
		for _, g := range raw {
			name, ok := index.GroupNames[g.GroupID]
			if !ok {
				// The GQL groupId is an ORN (o::<customerid>:rbacgroup:<coid>). Composing the OID
				// form o:::rbacgroup:<ORN> matches what the provider stores in state and is what
				// terraform import and plan will accept for this field.
				rawOID := "o:::rbacgroup:" + g.GroupID
				grants = append(grants, rewrite.Grant{RawSubject: rawOID, Role: g.Role})
				missing = append(missing, g.GroupID)
			} else {
				grants = append(grants, rewrite.Grant{GroupName: name, Role: g.Role})
			}
		}
		if len(missing) > 0 {
			rawIDEntries = append(rawIDEntries, fmt.Sprintf("%s (%q): group(s) %s not in state's observe_rbac_group resources — raw OID emitted, replace with var.rbac_groups[\"<name>\"].oid",
				id, names[id], strings.Join(missing, ", ")))
		}
		if len(grants) > 0 {
			out[id] = grants
		}
		fmt.Printf("\rgrants    %d/%d datasets", i+1, len(ids))
	}
	fmt.Println()
	return out, rawIDEntries, nil
}

// buildRefMap assembles oid resolution from state, the repo's module bindings, and the names just
// assigned to the datasets being imported.
//
// The self-map matters: the requested datasets may reference each other, and none of them is in
// state yet, so nothing else could resolve those oids.
func buildRefMap(defs map[string]*observe.TerraformDefinition, names map[string]string, index *tf.Index, repo *tf.Repo) *tf.Map {
	self := make(map[string]string, len(defs))
	for id, name := range names {
		self["o:::dataset:"+id] = name
	}
	return (&tf.Builder{Index: index, Repo: repo, SelfOIDs: self}).Build()
}

// rewriteAll converts every definition, in dataset-id order for deterministic output.
//
// A dataset that fails to rewrite is collected rather than aborting the batch: one malformed dataset
// out of a hundred should cost one dataset, not the whole run. The failures land in the report, which
// forces a non-zero exit.
func rewriteAll(cfg *Config, defs map[string]*observe.TerraformDefinition, names map[string]string, grants map[string][]rewrite.Grant, refs *tf.Map, freshnessDefault string) ([]*rewrite.Result, []string, error) {
	w := &rewrite.Rewriter{
		Refs:                  refs,
		AdoptFreshnessDefault: cfg.AdoptFreshnessDefault,
		FreshnessDefault:      freshnessDefault,
		EmitCorrelationTags:   cfg.EmitCorrelationTags,
	}

	ids := make([]string, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	results := make([]*rewrite.Result, 0, len(ids))
	var failures []string
	for _, id := range ids {
		res, err := w.Rewrite(defs[id], names[id], grants[id])
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		results = append(results, res)
	}
	return results, failures, nil
}

// destinationConflicts reports every `<name>.tf` that already exists and declares something other
// than the dataset (plus its correlation tags and grants companion) being written under that name.
//
// Without this check `os.WriteFile` would silently truncate. The module's own `main.tf`,
// `variables.tf` and `locals.tf` are the dangerous cases: they declare no resource, so the
// resource-name collision check cannot see them, yet a dataset whose importName is `main` would
// replace one wholesale. An update target — a file that already declares exactly this dataset — is
// not a conflict; that is the whole point of an update.
//
// An already-managed dataset id is skipped here entirely, not merely permitted: it is placed by
// planResult into the exact byte range its existing resource already occupies (see
// tf.Repo.ExistingDatasets), never by writing a whole file at the <name>.tf convention, so the risk
// this check exists for — silently overwriting a file's unrelated content — cannot arise for it
// regardless of what else that file declares.
//
// The repo root's `imports.tf` is a destination too, and it sits among the repo's hand-maintained
// root files, so it gets the same treatment: refuse to append into something that is not
// recognisably our own import blocks.
func destinationConflicts(cfg *Config, repo *tf.Repo, names map[string]string, alreadyManaged map[string]string) ([]string, error) {
	resourceNames := make([]string, 0, len(names))
	for id, name := range names {
		if _, managed := alreadyManaged[id]; managed {
			continue
		}
		resourceNames = append(resourceNames, name)
	}
	sort.Strings(resourceNames)

	var conflicts []string
	for _, name := range resourceNames {
		path := filepath.Join(repo.ModulePath(), name+".tf")
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		ours, err := tf.DeclaresOnlyDataset(path, name)
		if err != nil {
			return nil, err
		}
		if !ours {
			conflicts = append(conflicts, fmt.Sprintf("%s.tf already exists and declares something else", name))
		}
	}

	path := filepath.Join(repo.Root, output.ImportBlocksFile)
	if _, err := os.Stat(path); err == nil {
		ours, err := tf.DeclaresOnlyImports(path)
		if err != nil {
			return nil, err
		}
		if !ours {
			conflicts = append(conflicts,
				fmt.Sprintf("%s already exists and holds more than import blocks", output.ImportBlocksFile))
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return conflicts, nil
}

// writeAll writes the .tf files (per each result's already-resolved placement — a whole new file
// for a first-time adoption, or in-place splices for an already-managed dataset and/or its grants),
// the (already-computed) freshness_overrides update, runs terraform fmt, and emits the manifest,
// import blocks, and rollback script.
func writeAll(cfg *Config, repo *tf.Repo, results []*rewrite.Result, plans map[string]resultPlacement, manifest *output.Manifest, freshnessBytes []byte, freshnessChanged []string) error {
	written, err := writeResourceFiles(results, plans)
	if err != nil {
		return err
	}
	fmt.Printf("wrote     %d files to %s\n", len(written), cfg.ModuleDir)

	if len(freshnessChanged) > 0 {
		if err := os.WriteFile(repo.RootConfigPath, freshnessBytes, 0o644); err != nil {
			return err
		}
		fmt.Printf("patched   %d freshness_overrides entries in %s\n",
			len(freshnessChanged), filepath.Base(repo.RootConfigPath))
	}

	// Import blocks are terraform source in the repo, so they are written before fmt runs over the
	// root. Appended, never rewritten wholesale: this file accumulates across every run against this
	// repo, and WriteImportBlocks decides per entry whether anything is owed at all, so a run that
	// only updates already-managed datasets writes nothing here.
	path := filepath.Join(repo.Root, output.ImportBlocksFile)
	n, err := output.WriteImportBlocks(path, manifest, cfg.ImportGate)
	if err != nil {
		return err
	}
	switch {
	case n > 0:
		written = append(written, path)
		fmt.Printf("wrote     %d import block(s) to %s\n", n, output.ImportBlocksFile)
	default:
		fmt.Printf("skipped   %s: nothing new to import\n", output.ImportBlocksFile)
	}

	if err := terraformFmt(cfg.TerraformBin, append(written, repo.RootConfigPath)); err != nil {
		return err
	}

	manifestPath := filepath.Join(cfg.OutDir, "manifest.json")
	if err := manifest.Write(manifestPath); err != nil {
		return err
	}
	artifacts := []string{filepath.Base(manifestPath)}

	// Rollback is `terraform state rm`, scoped to whatever this run actually imported. An
	// already-managed dataset or grants resource was not freshly imported here, and rolling it back
	// would remove a resource this run had nothing to do with.
	rollbackPath := filepath.Join(cfg.OutDir, "rollback.sh")
	if err := output.WriteRollbackScript(rollbackPath, manifest); err != nil {
		return err
	}
	artifacts = append(artifacts, filepath.Base(rollbackPath))

	fmt.Printf("wrote     %s\n", strings.Join(artifacts, ", "))
	if cfg.ImportGate == "" {
		fmt.Printf("warning   import blocks are ungated: every workspace sharing this config will run\n")
		fmt.Printf("          them. Pass -import-gate if more than one tenant is applied from it.\n")
	}
	return nil
}

// computeFreshnessOverrides works out the update to the root module block's freshness_overrides map
// for this run's results, without writing it. Called unconditionally, including for a dry run, so the
// report can preview it; writeAll writes the bytes only when freshnessChanged is non-empty.
func computeFreshnessOverrides(repo *tf.Repo, moduleDir string, results []*rewrite.Result) (updatedBytes []byte, freshnessChanged []string, err error) {
	var entries []tf.FreshnessOverride
	for _, res := range results {
		if res.FreshnessOverride != "" {
			entries = append(entries, tf.FreshnessOverride{Key: res.ResourceName, Value: res.FreshnessOverride})
		}
	}
	if len(entries) == 0 {
		return nil, nil, nil
	}
	if !repo.HasFreshnessOverrides {
		return nil, nil, fmt.Errorf("%d datasets need a freshness override but the module block in %s passes no freshness_overrides map",
			len(entries), repo.RootConfigPath)
	}

	src, err := os.ReadFile(repo.RootConfigPath)
	if err != nil {
		return nil, nil, err
	}
	return tf.AddFreshnessOverrides(src, repo.RootConfigPath, moduleDir, entries)
}

// terraformFmt formats exactly the files given. CI gates on `terraform fmt -check`, so anything this
// tool writes has to be fmt-clean.
//
// Naming files rather than the module directory is deliberate. `terraform fmt <dir>` reformats every
// file in it, and 106 of the reference repo's 665 module files were already not fmt-clean, so a run
// rewrote 106 files it had not authored and the real diff was unreviewable until they were reverted by
// hand. Whether the repo is fmt-clean is not this tool's business.
func terraformFmt(bin string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	// One invocation: terraform fmt takes several paths, so this stays a single process regardless of
	// how many datasets were written.
	out, err := exec.Command(bin, append([]string{"fmt"}, files...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s fmt: %w\n%s", bin, err, out)
	}
	// fmt prints the files it changed. Reporting the count makes it visible when generated output was
	// not already clean, which would otherwise be silent.
	changed := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			changed++
		}
	}
	fmt.Printf("formatted %d of %d written files\n", changed, len(files))
	return nil
}

// replicaConflicts checks whether any dataset already exists in the replica tenant, where the
// imported config will also be applied.
//
// The comparison is on the Observe dataset name, not the terraform resource name: a name clash is
// what makes the replica apply fail or create a duplicate.
func replicaConflicts(path string, results []*rewrite.Result) ([]string, error) {
	state, err := tf.LoadState(path)
	if err != nil {
		return nil, fmt.Errorf("-target-state: %w", err)
	}
	index := tf.BuildIndex(state, "")

	var conflicts []string
	for _, res := range results {
		if _, exists := index.NamesToOID[res.DatasetName]; exists {
			conflicts = append(conflicts, res.DatasetName)
		}
	}
	sort.Strings(conflicts)
	return conflicts, nil
}

func datasetOIDs(defs map[string]*observe.TerraformDefinition) []string {
	out := make([]string, 0, len(defs))
	for id := range defs {
		out = append(out, "o:::dataset:"+id)
	}
	sort.Strings(out)
	return out
}
