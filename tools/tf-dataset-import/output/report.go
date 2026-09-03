package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Report summarizes a run for the operator, and doubles as the machine-readable signal a wrapper
// (e.g. the GitHub Action driving this) needs to decide what to do with the run: NeedsAttention
// covers only the fields that actually block or need a human decision. Updated and FreshnessUpdated
// are informational — a dataset already under management being kept in sync is success, not a
// problem, and forcing a non-zero exit for it would make "nothing to do" and "something needs fixing"
// indistinguishable to anything that only reads the exit code.
type Report struct {
	// Unresolved lists oids with no reference at all, keyed by oid, with the datasets that
	// referenced them. These need a module variable wired through the root config.
	Unresolved map[string][]string `json:"unresolved,omitempty"`
	// Review lists oids resolved to an in-module data source. The reference works, but its name is
	// scoped to whichever dashboard or monitor pulled the dataset in, so promoting it to a module
	// variable is usually the right follow-up.
	Review map[string][]string `json:"review,omitempty"`
	// FreshnessUpdated names freshness_overrides entries this run added or changed, with the old and
	// new value. A changed entry means Observe's current value differed from whatever was already in
	// the map — including a value someone tuned by hand — and this run's whole point is to keep it in
	// sync, so the update is applied, not just reported.
	FreshnessUpdated []string `json:"freshness_updated,omitempty"`
	// SynthesizedFreshness names the datasets that had no freshness of their own.
	SynthesizedFreshness []string `json:"synthesized_freshness,omitempty"`
	// CorrelationTags names the tag resources each dataset carries, keyed by dataset.
	CorrelationTags map[string][]string `json:"correlation_tags,omitempty"`
	// EmittedCorrelationTags records whether tag resources were written to the .tf files.
	EmittedCorrelationTags bool `json:"emitted_correlation_tags,omitempty"`
	// NameCollisions blocks the run before anything is written.
	NameCollisions []string `json:"name_collisions,omitempty"`
	// Clobbers names destination files that already exist and declare something other than what is
	// being written under that name. Also blocks the run.
	Clobbers []string `json:"clobbers,omitempty"`
	// Updated names datasets already under this repo's management before this run, refreshed from
	// their current live definition rather than assigned a new resource.
	Updated []string `json:"updated,omitempty"`
	// Unadoptable names datasets that cannot be an observe_dataset resource at all, with the reason.
	Unadoptable []string `json:"unadoptable,omitempty"`
	// Failed holds per-dataset rewrite errors. The rest of the batch still ran.
	Failed []string `json:"failed,omitempty"`
	// UnknownResources maps a dropped resource address to the datasets that carried it.
	UnknownResources map[string][]string `json:"unknown_resources,omitempty"`
	// GrantsWithRawIDs names datasets where one or more grants could not be resolved to a
	// var.rbac_groups reference. The observe_resource_grants block was still emitted but contains
	// a literal OID string for those subjects. The plan is usable but the reference is tenant-specific;
	// the operator should add the group to the rbac_groups for_each set and replace the raw OID.
	GrantsWithRawIDs []string `json:"grants_with_raw_ids,omitempty"`
	// TargetStateConflicts names datasets that already exist in the replica tenant.
	TargetStateConflicts []string `json:"target_state_conflicts,omitempty"`

	BlastRadius  int `json:"blast_radius,omitempty"`
	DatasetCount int `json:"dataset_count"`
	CacheHits    int `json:"cache_hits"`
	CacheMisses  int `json:"cache_misses"`

	// NeedsAttentionResult mirrors NeedsAttention() at the top level of the JSON, so a wrapper can
	// branch on one boolean field instead of re-deriving the same logic this method encodes.
	NeedsAttentionResult bool `json:"needs_attention"`
}

// NewReport returns an empty report.
func NewReport() *Report {
	return &Report{
		Unresolved:       map[string][]string{},
		Review:           map[string][]string{},
		CorrelationTags:  map[string][]string{},
		UnknownResources: map[string][]string{},
	}
}

// Add folds one dataset's manifest entry into the report.
func (r *Report) Add(e Entry) {
	r.DatasetCount++
	for _, oid := range e.UnresolvedRefs {
		r.Unresolved[oid] = append(r.Unresolved[oid], e.ResourceName)
	}
	for oid := range e.ReviewRefs {
		r.Review[oid] = append(r.Review[oid], e.ResourceName)
	}
	if e.FreshnessSynthesized {
		r.SynthesizedFreshness = append(r.SynthesizedFreshness, e.ResourceName)
	}
	if len(e.CorrelationTags) > 0 {
		r.CorrelationTags[e.ResourceName] = e.CorrelationTags
	}
	for _, addr := range e.UnknownResources {
		r.UnknownResources[addr] = append(r.UnknownResources[addr], e.ResourceName)
	}
}

// NeedsAttention reports whether the run found something the operator must act on. The process
// exits non-zero in that case, so an unwired input cannot be missed.
//
// Updated and FreshnessUpdated are deliberately excluded: a dataset already under management being
// refreshed from its current live definition is this tool doing its job, not a defect, and a
// wrapper deciding whether to open or update a PR needs "successfully synced" and "something is
// wrong" to read as different outcomes.
func (r *Report) NeedsAttention() bool {
	return len(r.Unresolved) > 0 || len(r.Review) > 0 ||
		len(r.NameCollisions) > 0 || len(r.TargetStateConflicts) > 0 ||
		len(r.Clobbers) > 0 || len(r.Failed) > 0 || len(r.CorrelationTags) > 0 ||
		len(r.UnknownResources) > 0 || len(r.Unadoptable) > 0 || len(r.GrantsWithRawIDs) > 0
}

// WriteJSON serializes the report to path, so a wrapper driving this tool (a GitHub Action, say) has
// a machine-readable way to tell "nothing to do" apart from "a name collision needs -name-overrides"
// apart from "an unresolved reference needs a module variable" — all of which currently share exit
// code 3 via NeedsAttention, but mean different things for whether to open a PR, skip silently, or
// fail the job and alert someone.
func (r *Report) WriteJSON(path string) error {
	r.NeedsAttentionResult = r.NeedsAttention()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Write renders the report.
func (r *Report) Write(w io.Writer) {
	fmt.Fprintf(w, "\n%s\n", strings.Repeat("=", 72))
	fmt.Fprintf(w, "%d datasets processed  (cache: %d hit, %d fetched)\n",
		r.DatasetCount, r.CacheHits, r.CacheMisses)
	fmt.Fprintf(w, "%s\n", strings.Repeat("=", 72))

	if len(r.NameCollisions) > 0 {
		section(w, "RESOURCE NAME COLLISIONS — nothing was written")
		fmt.Fprintln(w, "Settle each with an entry in the -name-overrides file, then re-run.")
		fmt.Fprintln(w, "The response cache makes the re-run free.")
		for _, c := range r.NameCollisions {
			fmt.Fprintf(w, "  %s\n", c)
		}
	}

	if len(r.Clobbers) > 0 {
		section(w, "DESTINATION FILES ALREADY EXIST — nothing was written")
		fmt.Fprintln(w, "Each of these would have been overwritten. Move the file aside, or pick another")
		fmt.Fprintln(w, "resource name in -name-overrides, then re-run.")
		for _, c := range r.Clobbers {
			fmt.Fprintf(w, "  %s\n", c)
		}
	}

	if len(r.Failed) > 0 {
		section(w, "FAILED — these datasets produced no file")
		fmt.Fprintln(w, "The rest of the batch was still written.")
		for _, f := range r.Failed {
			fmt.Fprintf(w, "  %s\n", f)
		}
	}

	if len(r.Updated) > 0 {
		section(w, "ALREADY MANAGED — refreshed, not created")
		fmt.Fprintln(w, "The module already declared these datasets, so each was regenerated from its current")
		fmt.Fprintln(w, "live definition under its existing resource name rather than assigned a new one. If")
		fmt.Fprintln(w, "nothing changed in Observe, the file is byte-identical and this is a no-op.")
		for _, a := range r.Updated {
			fmt.Fprintf(w, "  %s\n", a)
		}
	}

	if len(r.Unadoptable) > 0 {
		section(w, "CANNOT BE AN observe_dataset — skipped")
		fmt.Fprintln(w, "These have no inputs, so they are not transforms: a reference table whose rows are")
		fmt.Fprintln(w, "uploaded, or a raw ingest target fed by a datastream. `inputs` is Required on the")
		fmt.Fprintln(w, "resource, so no generated config would apply. Each needs observe_reference_table,")
		fmt.Fprintln(w, "observe_source_dataset, or a data source referring to it — which one is a decision")
		fmt.Fprintln(w, "about intent, not something to derive.")
		for _, u := range r.Unadoptable {
			fmt.Fprintf(w, "  %s\n", u)
		}
	}

	if len(r.Unresolved) > 0 {
		section(w, "UNWIRED INPUTS — these oids have no reference in the module")
		fmt.Fprintln(w, "The raw oid was left in place, which pins the config to one tenant.")
		fmt.Fprintln(w, "Add a module variable and bind it in the root config, then re-run.")
		writeOIDGroups(w, r.Unresolved)
	}

	if len(r.Review) > 0 {
		section(w, "REVIEW — resolved to an in-module data source")
		fmt.Fprintln(w, "These references work, but the data source names are scoped to the dashboard")
		fmt.Fprintln(w, "or monitor that pulled the dataset in. Consider promoting them to module")
		fmt.Fprintln(w, "variables.")
		writeOIDGroups(w, r.Review)
	}

	if len(r.CorrelationTags) > 0 {
		r.writeCorrelationTags(w)
	}

	if len(r.UnknownResources) > 0 {
		section(w, "UNRECOGNIZED RESOURCES — dropped from the output")
		fmt.Fprintln(w, "The generator emitted resource types this tool does not handle. They are not in")
		fmt.Fprintln(w, "the generated files, so whatever they configure is left unmanaged.")
		writeOIDGroups(w, r.UnknownResources)
	}

	if len(r.TargetStateConflicts) > 0 {
		section(w, "REPLICA CONFLICTS — these dataset names already exist in the target state")
		fmt.Fprintln(w, "Applying to the replica would error or create a duplicate.")
		for _, name := range r.TargetStateConflicts {
			fmt.Fprintf(w, "  %s\n", name)
		}
	}

	if len(r.FreshnessUpdated) > 0 {
		section(w, "FRESHNESS_OVERRIDES UPDATED")
		fmt.Fprintln(w, "These entries were added or changed to match Observe's current value — including any")
		fmt.Fprintln(w, "that were already set to something else, since keeping this map in sync is this run's job.")
		for _, d := range r.FreshnessUpdated {
			fmt.Fprintf(w, "  %s\n", d)
		}
	}

	if len(r.SynthesizedFreshness) > 0 {
		section(w, "NO FRESHNESS OF THEIR OWN")
		fmt.Fprintln(w, "These datasets have no freshnessDesired. The lookup was emitted commented out")
		fmt.Fprintln(w, "so the post-import plan stays clean; uncomment to adopt the module default.")
		for _, name := range r.SynthesizedFreshness {
			fmt.Fprintf(w, "  %s\n", name)
		}
	}

	if len(r.GrantsWithRawIDs) > 0 {
		section(w, "GRANTS WITH RAW OIDs — manual wiring needed")
		fmt.Fprintln(w, "These datasets have RBAC grants for groups not in the state's observe_rbac_group")
		fmt.Fprintln(w, "for_each resource. The observe_resource_grants block was still written but contains")
		fmt.Fprintln(w, "a literal OID string for those subjects — the block works, but the reference is")
		fmt.Fprintln(w, "tenant-specific. To make it portable: add the group to the rbac_groups for_each set,")
		fmt.Fprintln(w, "then replace the raw OID with var.rbac_groups[\"<name>\"].oid and re-run.")
		for _, s := range r.GrantsWithRawIDs {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}

	if r.BlastRadius > 0 {
		section(w, "BLAST RADIUS")
		fmt.Fprintf(w, "%d managed datasets sit downstream of the imported set. Any config change to an\n", r.BlastRadius)
		fmt.Fprintf(w, "imported dataset recomputes its oid, which cascades an update to all of them.\n")
		fmt.Fprintf(w, "Datasets not managed by this repo are not counted; the real number is higher.\n")
	}

	if !r.NeedsAttention() {
		section(w, "Every oid resolved to a reference. No collisions, no correlation tags.")
	}
	fmt.Fprintln(w)
}

// writeCorrelationTags explains what happens to the tags, which differs by whether they were emitted.
//
// `observe_correlation_tag` declares no Importer, so it cannot be brought into state next to its
// dataset, and the backend rejects re-adding a tag that already exists. Neither option is a no-op, so
// the operator has to know which one they took.
func (r *Report) writeCorrelationTags(w io.Writer) {
	names := make([]string, 0, len(r.CorrelationTags))
	total := 0
	for name, tags := range r.CorrelationTags {
		names = append(names, name)
		total += len(tags)
	}
	sort.Strings(names)

	if r.EmittedCorrelationTags {
		section(w, "CORRELATION TAGS — emitted, and they will NOT plan clean")
		fmt.Fprintf(w, "%d tag resources across %d datasets were written.\n", total, len(names))
		fmt.Fprintln(w, "They cannot be imported (the provider declares no importer for them), so the")
		fmt.Fprintln(w, "post-import plan will show them as creates. Applying those against a tenant that")
		fmt.Fprintln(w, "already has the tags fails: the backend rejects a duplicate tag rather than")
		fmt.Fprintln(w, "treating it as a no-op. Drop -emit-correlation-tags unless the target tenant")
		fmt.Fprintln(w, "genuinely lacks them.")
	} else {
		section(w, "CORRELATION TAGS — not emitted, left unmanaged")
		fmt.Fprintf(w, "%d tags across %d datasets exist on the live datasets and are not in the\n", total, len(names))
		fmt.Fprintln(w, "generated config, so the post-import plan stays a no-op. They cannot be imported,")
		fmt.Fprintln(w, "and re-creating an existing tag is an error, so adopting them needs a separate")
		fmt.Fprintln(w, "decision. -emit-correlation-tags writes them anyway.")
	}
	for _, name := range names {
		fmt.Fprintf(w, "  %s: %s\n", name, strings.Join(r.CorrelationTags[name], ", "))
	}
}

func section(w io.Writer, title string) {
	fmt.Fprintf(w, "\n%s\n%s\n", title, strings.Repeat("-", len(title)))
}

// writeOIDGroups prints each key with the sorted, de-duplicated datasets referencing it.
func writeOIDGroups(w io.Writer, groups map[string][]string) {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "  %s\n      referenced by: %s\n", k, strings.Join(dedupeSorted(groups[k]), ", "))
	}
}

func dedupeSorted(in []string) []string {
	sort.Strings(in)
	out := in[:0:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
