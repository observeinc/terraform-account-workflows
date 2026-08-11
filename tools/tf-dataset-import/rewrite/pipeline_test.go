package rewrite

import (
	"context"
	"regexp"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"tfdatasetimport/observe"
)

// TestPipelineValuesAreByteIdentical is the tool's most important invariant.
//
// A pipeline is arbitrary OPAL, and the provider's diffSuppressPipeline trims only *trailing*
// whitespace, so any other change to the value produces a real diff — and, through
// datasetRecomputeOID, cascades an update to every downstream dataset. The rewrite is therefore
// only allowed to change how a pipeline is laid out in the file, never what it parses to — with
// one deliberate exception: a line that is only whitespace is normalized to truly empty (see
// stripBlankPipelineLines). Confirmed against a real `terraform plan`, where the state side of a
// blank separator line was genuinely empty and the generated config side carried the padding — so
// this normalization is what makes the value match the live dataset, not a deviation from it. The
// comparison below applies the same normalization to `want` before checking equality, isolating
// that one allowed change from everything else, which still must be byte-for-byte identical.
func TestPipelineValuesAreByteIdentical(t *testing.T) {
	source := &observe.DirSource{Dir: goldenDir}
	h := newHarness(t, "99010001", "99010002")

	for _, tc := range []struct{ id, name string }{
		{"99010001", "new_one"},
		{"99010002", "new_two"},
		{"99010004", "github_events"},
		{"99010005", "correlation_tags"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			def, err := source.GetDatasetTerraform(context.Background(), tc.id)
			if err != nil {
				t.Fatal(err)
			}

			w := &Rewriter{Refs: h.refs, FreshnessDefault: "5m"}
			res, err := w.Rewrite(def, tc.name, nil)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}

			want := pipelineValues(t, def.Resource)
			got := pipelineValues(t, string(res.HCL))

			if len(want) == 0 {
				t.Fatal("fixture has no pipelines to compare")
			}
			if len(got) != len(want) {
				t.Fatalf("got %d pipelines, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != blankLinesToEmpty(want[i]) {
					t.Errorf("stage %d pipeline changed by more than blank-line normalization.\n--- want (normalized) ---\n%q\n--- got ---\n%q",
						i, blankLinesToEmpty(want[i]), got[i])
				}
			}
		})
	}
}

// blankLinesToEmpty mirrors stripHeredocBlankLines at the value level, for comparison purposes: any
// line consisting only of whitespace becomes truly empty.
var whitespaceOnlyLine = regexp.MustCompile(`(?m)^[ \t]+$`)

func blankLinesToEmpty(s string) string {
	return whitespaceOnlyLine.ReplaceAllString(s, "")
}

// TestStripHeredocBlankLinesLeavesRealContentAlone confirms the strip is line-scoped: only a line
// that is nothing but whitespace is touched, and only that line's own token is replaced.
//
// The separator line's trailing whitespace is built with explicit escapes, rather than a raw
// string literal with literal trailing spaces, so a trailing-whitespace-trimming editor cannot
// silently remove the very thing this test needs to reproduce the bug.
func TestStripHeredocBlankLinesLeavesRealContentAlone(t *testing.T) {
	src := "resource \"observe_dataset\" \"y\" {\n" +
		"  stage {\n" +
		"    pipeline = <<-EOT\n" +
		"            filter true\n" +
		"            \n" + // whitespace-only separator line — the bug this test reproduces
		"            make_col a: 1\n" +
		"        EOT\n" +
		"  }\n" +
		"}\n"
	f, diags := hclwrite.ParseConfig([]byte(src), "t.tf", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	body := f.Body().Blocks()[0].Body().Blocks()[0].Body()
	attr := body.GetAttribute("pipeline")

	out, changed := stripHeredocBlankLines(attr.Expr().BuildTokens(nil))
	if !changed {
		t.Fatal("expected the whitespace-only line to be stripped")
	}
	body.SetAttributeRaw("pipeline", out)

	got := evalPipelineValue(t, f.Bytes())
	want := "filter true\n\nmake_col a: 1\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestStripHeredocBlankLinesIsANoOpWhenAlreadyEmpty guards against churn: a heredoc with no
// whitespace-only lines is reported unchanged, so callers can skip rewriting the attribute.
func TestStripHeredocBlankLinesIsANoOpWhenAlreadyEmpty(t *testing.T) {
	const src = `resource "x" "y" {
  stage {
    pipeline = <<-EOT
            filter true

            make_col a: 1
        EOT
  }
}
`
	f, diags := hclwrite.ParseConfig([]byte(src), "t.tf", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	body := f.Body().Blocks()[0].Body().Blocks()[0].Body()
	attr := body.GetAttribute("pipeline")

	_, changed := stripHeredocBlankLines(attr.Expr().BuildTokens(nil))
	if changed {
		t.Error("a heredoc with no whitespace-only lines should report no change")
	}
}

// evalPipelineValue parses a config and returns the single stage pipeline's string value.
func evalPipelineValue(t *testing.T, src []byte) string {
	t.Helper()
	vals := pipelineValues(t, string(src))
	if len(vals) != 1 {
		t.Fatalf("got %d pipelines, want 1", len(vals))
	}
	return vals[0]
}

// pipelineValues evaluates every stage pipeline in a config to its string value, which is what the
// provider compares. Layout differences vanish at this level; content differences do not.
func pipelineValues(t *testing.T, src string) []string {
	t.Helper()

	f, diags := hclsyntax.ParseConfig([]byte(src), "cmp.tf", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		t.Fatal("unexpected body type")
	}

	var out []string
	for _, block := range body.Blocks {
		if block.Type != "resource" || len(block.Labels) != 2 || block.Labels[0] != "observe_dataset" {
			continue
		}
		for _, stage := range block.Body.Blocks {
			if stage.Type != "stage" {
				continue
			}
			attr, present := stage.Body.Attributes["pipeline"]
			if !present {
				continue
			}
			v, d := attr.Expr.Value(nil)
			if d.HasErrors() || v.IsNull() || v.Type() != cty.String {
				t.Fatalf("pipeline is not a string literal: %s", d.Error())
			}
			out = append(out, v.AsString())
		}
	}
	return out
}
