// Flag parsing and validation. This lives in package main because the CLI surface is the only thing
// that consumes it.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// Config is the fully resolved, validated run configuration.
type Config struct {
	DatasetIDs []string

	CustomerID string
	Domain     string
	Token      string

	Repo        string
	ModuleDir   string
	StatePath   string
	TargetState string

	// TFWorkspace, if set, is baked into rollback.sh's workspace guard. Optional: import blocks need
	// no workspace of their own (the wrapper already selected one before invoking this tool), so this
	// only matters if rollback.sh is ever run by hand.
	TFWorkspace   string
	NameOverrides map[string]string

	// AdoptFreshnessDefault emits an active freshness lookup for a dataset that has none, rather
	// than a commented-out one. That sets a freshness where the dataset had no
	// freshnessDesired, changing its materialization cadence and recomputing the oid of every
	// dependent — fine as a follow-up, but it would stop the post-import plan being a no-op.
	AdoptFreshnessDefault bool
	// EmitCorrelationTags writes an observe_correlation_tag resource per tag. Off by default; see
	// rewrite.Rewriter.EmitCorrelationTags for why.
	EmitCorrelationTags bool

	// ImportGate is an HCL boolean expression true only in the workspace holding the source tenant.
	// See output.WriteImportBlocks for what it guards against.
	ImportGate string

	CacheDir     string
	TerraformBin string
	OutDir       string
	DryRun       bool
}

// MetaURL is the GraphQL endpoint for the source tenant.
func (c *Config) MetaURL() string {
	return fmt.Sprintf("https://%s.%s/v1/meta", c.CustomerID, c.Domain)
}

// ModuleAddress is the terraform address prefix for imports, derived from the module directory's
// base name. `modules/example` yields `module.example`.
func (c *Config) ModuleAddress() string {
	return "module." + filepath.Base(c.ModuleDir)
}

type flags struct {
	datasetIDs            string
	datasetIDsFile        string
	profile               string
	customerID            string
	domain                string
	token                 string
	repo                  string
	moduleDir             string
	state                 string
	targetState           string
	tfWorkspace           string
	nameOverrides         string
	adoptFreshnessDefault bool
	emitCorrelationTags   bool
	importGate            string
	cacheDir              string
	terraformBin          string
	out                   string
	dryRun                bool
}

// Parse resolves command-line args into a validated Config.
func parseFlags(args []string) (*Config, error) {
	var f flags
	fs := flag.NewFlagSet("tf-dataset-import", flag.ContinueOnError)
	fs.StringVar(&f.datasetIDs, "dataset-ids", "", "comma-separated source dataset ids")
	fs.StringVar(&f.datasetIDsFile, "dataset-ids-file", "", "file of dataset ids, one per line (# comments allowed)")
	fs.StringVar(&f.profile, "profile", "", "profile name in ~/.observe/config.json supplying customer id, domain and token")
	fs.StringVar(&f.customerID, "customer-id", "", "source tenant customer id (overrides -profile)")
	fs.StringVar(&f.domain, "domain", "", "source tenant domain, e.g. observeinc.com (overrides -profile)")
	fs.StringVar(&f.token, "token", "", "bearer token (overrides -profile; prefer OBSERVE_TOKEN)")
	fs.StringVar(&f.repo, "repo", "", "path to the terraform account repo")
	fs.StringVar(&f.moduleDir, "module-dir", "modules/observe", "repo-relative module directory to write into")
	fs.StringVar(&f.state, "state", "", "source tenant terraform state file")
	fs.StringVar(&f.targetState, "target-state", "", "optional replica state file, for a dataset-name collision pre-flight")
	fs.StringVar(&f.tfWorkspace, "tf-workspace", "", "terraform workspace name baked into rollback.sh's workspace guard (optional)")
	fs.StringVar(&f.nameOverrides, "name-overrides", "", "optional JSON file mapping dataset id to terraform resource name")
	fs.BoolVar(&f.adoptFreshnessDefault, "adopt-freshness-default", false,
		"where a dataset has no freshness, emit an active lookup instead of a commented-out one")
	fs.BoolVar(&f.emitCorrelationTags, "emit-correlation-tags", false,
		"emit observe_correlation_tag resources; they cannot be imported and error on apply if the tag already exists")
	fs.StringVar(&f.importGate, "import-gate", "",
		"HCL boolean true only in the source tenant's workspace, e.g. local.in_west (needs Terraform 1.7)")
	fs.StringVar(&f.cacheDir, "cache-dir", "", "on-disk getTerraform response cache (default <out>/cache)")
	fs.StringVar(&f.terraformBin, "terraform-bin", "terraform", "terraform binary used for fmt")
	fs.StringVar(&f.out, "out", "./out", "output directory for manifest.json, report.json and rollback.sh")
	fs.BoolVar(&f.dryRun, "dry-run", false, "resolve and report without writing any file")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return f.resolve()
}

func (f *flags) resolve() (*Config, error) {
	c := &Config{
		CustomerID:            f.customerID,
		Domain:                f.domain,
		Token:                 f.token,
		Repo:                  f.repo,
		ModuleDir:             filepath.Clean(f.moduleDir),
		StatePath:             f.state,
		TargetState:           f.targetState,
		TFWorkspace:           f.tfWorkspace,
		AdoptFreshnessDefault: f.adoptFreshnessDefault,
		EmitCorrelationTags:   f.emitCorrelationTags,
		ImportGate:            strings.TrimSpace(f.importGate),
		TerraformBin:          f.terraformBin,
		OutDir:                f.out,
		DryRun:                f.dryRun,
	}

	var errs []error

	if c.ImportGate != "" {
		// Catching a malformed expression here beats letting terraform reject the generated file
		// after the .tf files have already been written.
		if _, diags := hclsyntax.ParseExpression([]byte(c.ImportGate), "-import-gate", hcl.InitialPos); diags.HasErrors() {
			errs = append(errs, fmt.Errorf("-import-gate is not a valid HCL expression: %s", diags.Error()))
		}
	}

	ids, err := readDatasetIDs(f.datasetIDs, f.datasetIDsFile)
	if err != nil {
		errs = append(errs, err)
	}
	c.DatasetIDs = ids

	if f.profile != "" {
		if err := applyProfile(c, f.profile); err != nil {
			errs = append(errs, err)
		}
	}
	if c.Token == "" {
		c.Token = os.Getenv("OBSERVE_TOKEN")
	}

	if f.nameOverrides != "" {
		overrides, err := readNameOverrides(f.nameOverrides)
		if err != nil {
			errs = append(errs, err)
		}
		c.NameOverrides = overrides
	}

	for _, req := range []struct{ name, value string }{
		{"-customer-id (or -profile)", c.CustomerID},
		{"-domain (or -profile)", c.Domain},
		{"-token (or -profile, or OBSERVE_TOKEN)", c.Token},
		{"-repo", c.Repo},
		{"-state", c.StatePath},
	} {
		if req.value == "" {
			errs = append(errs, fmt.Errorf("%s is required", req.name))
		}
	}

	c.CacheDir = f.cacheDir
	if c.CacheDir == "" {
		c.CacheDir = filepath.Join(c.OutDir, "cache")
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// readDatasetIDs parses ids from the inline flag or a file.
//
// Comments are stripped per line, before splitting: cutting at the first `#` after tokenizing would
// only drop the `#` itself and turn the rest of the comment into dataset ids. Every id must be all
// digits, which catches that class of mistake at the source instead of as a GraphQL error a hundred
// lines later.
func readDatasetIDs(inline, path string) ([]string, error) {
	if (inline == "") == (path == "") {
		return nil, errors.New("exactly one of -dataset-ids or -dataset-ids-file is required")
	}

	raw := inline
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("-dataset-ids-file: %w", err)
		}
		raw = string(b)
	}

	seen := make(map[string]bool)
	var ids []string
	var bad []string
	for _, line := range strings.Split(raw, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, field := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == '\r' || r == ' ' || r == '\t'
		}) {
			if !isAllDigits(field) {
				bad = append(bad, field)
				continue
			}
			if seen[field] {
				continue
			}
			seen[field] = true
			ids = append(ids, field)
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("dataset ids must be numeric, got %s", strings.Join(bad, ", "))
	}
	if len(ids) == 0 {
		return nil, errors.New("no dataset ids given")
	}
	return ids, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func readNameOverrides(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-name-overrides: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("-name-overrides: %w", err)
	}
	return m, nil
}

// observeConfig is the subset of ~/.observe/config.json this tool reads.
type observeConfig struct {
	Profiles map[string]struct {
		CustomerID string `json:"customerId"`
		Domain     string `json:"domain"`
		Token      string `json:"token"`
	} `json:"profiles"`
}

// applyProfile fills any credential field the flags left empty from the observe CLI config, so
// explicit flags always win.
func applyProfile(c *Config, name string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".observe", "config.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("-profile %s: %w", name, err)
	}
	var oc observeConfig
	if err := json.Unmarshal(b, &oc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	p, ok := oc.Profiles[name]
	if !ok {
		names := make([]string, 0, len(oc.Profiles))
		for k := range oc.Profiles {
			names = append(names, k)
		}
		return fmt.Errorf("-profile %s not found in %s (have: %s)", name, path, strings.Join(names, ", "))
	}
	if c.CustomerID == "" {
		c.CustomerID = p.CustomerID
	}
	if c.Domain == "" {
		c.Domain = p.Domain
	}
	if c.Token == "" {
		c.Token = p.Token
	}
	return nil
}
