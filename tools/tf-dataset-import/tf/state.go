// Package tf models the terraform side of an import: reading a state file, reading and editing an
// account repo's config, and resolving hardcoded Observe oids to portable HCL references.
//
// The three belong together because the last needs both of the others — a reference is only portable
// if some object in the repo's config carries the oid that state says it has.
package tf

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// State is the subset of terraform state version 4 this tool needs.
type State struct {
	Version   int        `json:"version"`
	Resources []Resource `json:"resources"`
}

// Resource is one state entry. Module is empty for the root module, otherwise `module.<name>`.
type Resource struct {
	Module    string     `json:"module"`
	Mode      string     `json:"mode"` // "managed" or "data"
	Type      string     `json:"type"`
	Name      string     `json:"name"`
	Instances []Instance `json:"instances"`
}

// Instance is one instance of a resource.
type Instance struct {
	IndexKey   any            `json:"index_key"` // for_each key: string or int
	Attributes map[string]any `json:"attributes"`
}

// Address is the terraform address of the resource, without any instance index.
func (r *Resource) Address() string {
	var b strings.Builder
	if r.Module != "" {
		b.WriteString(r.Module)
		b.WriteString(".")
	}
	if r.Mode == "data" {
		b.WriteString("data.")
	}
	b.WriteString(r.Type)
	b.WriteString(".")
	b.WriteString(r.Name)
	return b.String()
}

// attrs returns the first instance's attributes, or nil when the resource has no instances (a
// `count = 0` data source, for example).
func (r *Resource) attrs() map[string]any {
	if len(r.Instances) == 0 {
		return nil
	}
	return r.Instances[0].Attributes
}

func str(attrs map[string]any, key string) string {
	if attrs == nil {
		return ""
	}
	s, _ := attrs[key].(string)
	return s
}

// Load reads and decodes a state file.
func LoadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode state %s: %w", path, err)
	}
	if s.Version != 4 {
		return nil, fmt.Errorf("state %s: want version 4, got %d", path, s.Version)
	}
	return &s, nil
}

// NormalizeOID strips the trailing `/<version>` from an oid.
//
// The same dataset appears in state both ways: an `oid` attribute carries the version suffix
// (`o:::dataset:99015371/2026-01-05T17:07:19Z`) while generated `inputs` are bare
// (`o:::dataset:99007951`). Everything is keyed on the bare form so the two match.
func NormalizeOID(oid string) string {
	if i := strings.IndexByte(oid, '/'); i >= 0 {
		return oid[:i]
	}
	return oid
}

// IsOID reports whether s looks like an Observe oid: `o:<workspace>:<folder>:<type>:<id>`, in
// practice almost always `o:::<type>:<id>`.
func IsOID(s string) bool {
	return strings.HasPrefix(s, "o:") && strings.Count(s, ":") >= 4
}
