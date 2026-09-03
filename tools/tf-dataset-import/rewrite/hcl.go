// Package rewrite turns generated terraform into repo-conventional terraform.
//
// Generated output is a `terraform show` of a *data source*: it carries hardcoded oids and a couple
// of attributes a resource cannot accept. The target is a *resource* in a module that references
// everything through variables and peers. Only what has to change is changed — the generated block is
// mutated in place, so any attribute the tool has no opinion about is passed through untouched.
package rewrite

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// tokensForExpr builds tokens for an arbitrary HCL expression written as source text.
//
// hclwrite exposes builders only for values, traversals and function calls, none of which cover
// an indexed reference like `data.observe_dataset.x[0].oid`. Lexing the source and re-emitting its
// tokens handles every form uniformly.
func tokensForExpr(src string) (hclwrite.Tokens, error) {
	lexed, diags := hclsyntax.LexExpression([]byte(src), "", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("lex %q: %s", src, diags.Error())
	}
	out := make(hclwrite.Tokens, 0, len(lexed))
	for _, t := range lexed {
		if t.Type == hclsyntax.TokenEOF {
			continue
		}
		out = append(out, &hclwrite.Token{Type: t.Type, Bytes: t.Bytes})
	}
	return out, nil
}

// mustQuote renders s as an HCL string literal, escaping as needed.
func mustQuote(s string) string {
	return string(hclwrite.TokensForValue(cty.StringVal(s)).Bytes())
}

// attrExpr returns an attribute's expression source text, or "" when the attribute is absent.
func attrExpr(body *hclwrite.Body, name string) string {
	attr := body.GetAttribute(name)
	if attr == nil {
		return ""
	}
	return string(attr.Expr().BuildTokens(nil).Bytes())
}

// attrString reads an attribute as a string literal. present is false when the attribute is
// absent or explicitly null.
func attrString(body *hclwrite.Body, name string) (value string, present bool) {
	src := attrExpr(body, name)
	if src == "" {
		return "", false
	}
	expr, diags := hclsyntax.ParseExpression([]byte(src), name, hcl.InitialPos)
	if diags.HasErrors() {
		return "", false
	}
	v, diags := expr.Value(nil)
	if diags.HasErrors() || v.IsNull() || v.Type() != cty.String {
		return "", false
	}
	return v.AsString(), true
}

// isNullOrAbsent reports whether an attribute is missing or literally null. Generated output uses
// explicit nulls for unset optional attributes (`description = null`).
func isNullOrAbsent(body *hclwrite.Body, name string) bool {
	src := attrExpr(body, name)
	if src == "" {
		return true
	}
	expr, diags := hclsyntax.ParseExpression([]byte(src), name, hcl.InitialPos)
	if diags.HasErrors() {
		return false
	}
	v, diags := expr.Value(nil)
	if diags.HasErrors() {
		return false
	}
	return v.IsNull()
}

// mapEntry is one key/value pair from a map-literal attribute, with values kept as source text so
// a non-literal value survives untouched.
type mapEntry struct {
	Key   string
	Value string
}

// parseMapAttr reads a map-literal attribute into ordered entries. Order follows the source, which
// generated output emits alphabetically.
func parseMapAttr(body *hclwrite.Body, name string) ([]mapEntry, bool, error) {
	src := attrExpr(body, name)
	if src == "" {
		return nil, false, nil
	}
	expr, diags := hclsyntax.ParseExpression([]byte(src), name, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, false, fmt.Errorf("parse %s: %s", name, diags.Error())
	}
	cons, ok := expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return nil, false, nil
	}

	entries := make([]mapEntry, 0, len(cons.Items))
	for _, item := range cons.Items {
		kv, kd := item.KeyExpr.Value(nil)
		if kd.HasErrors() || kv.Type() != cty.String {
			return nil, false, fmt.Errorf("%s has a non-literal key", name)
		}
		valRange := item.ValueExpr.Range()
		entries = append(entries, mapEntry{
			Key:   kv.AsString(),
			Value: string(valRange.SliceBytes([]byte(src))),
		})
	}
	return entries, true, nil
}
