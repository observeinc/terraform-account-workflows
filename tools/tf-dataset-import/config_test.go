package main

import (
	"strings"
	"testing"
)

// baseArgs are the flags every run requires, so a test can vary one thing at a time.
func baseArgs(extra ...string) []string {
	return append([]string{
		"-dataset-ids", "123",
		"-customer-id", "102",
		"-domain", "observeinc.com",
		"-token", "t",
		"-repo", "/tmp/repo",
		"-state", "/tmp/state.json",
	}, extra...)
}

// TestImportGateFlag covers the validation that keeps a malformed -import-gate expression from being
// discovered by terraform only after the .tf files have already been written. -tf-workspace is
// intentionally absent from baseArgs, to also cover that it is no longer required.
func TestImportGateFlag(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string // substring; empty means the flags should parse
		check   func(*testing.T, *Config)
	}{
		{
			name: "no gate is allowed",
			args: baseArgs(),
			check: func(t *testing.T, c *Config) {
				if c.ImportGate != "" {
					t.Errorf("gate = %q", c.ImportGate)
				}
			},
		},
		{
			name: "a gate is trimmed, since it is interpolated straight into HCL",
			args: baseArgs("-import-gate", " local.in_west "),
			check: func(t *testing.T, c *Config) {
				if c.ImportGate != "local.in_west" {
					t.Errorf("gate = %q", c.ImportGate)
				}
			},
		},
		{
			name:    "gate that is not valid HCL",
			args:    baseArgs("-import-gate", "local.in_west ??"),
			wantErr: "not a valid HCL expression",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseFlags(tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got none", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			tc.check(t, cfg)
		})
	}
}
