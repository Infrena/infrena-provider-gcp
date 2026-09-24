package mmv1

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ignoredResourceKeys are the resource-level keys this package deliberately
// does not read, each with the reason. Everything else in the corpus must be
// a field of Resource. A key missing from both is a key nobody decided about,
// which is how resource-level `immutable:` went unread on 178 resources.
var ignoredResourceKeys = map[string]string{
	// Terraform's docs, tests and code generation.
	"docs": "tf docs", "samples": "tf examples", "examples": "tf examples",
	"sweeper": "tf test cleanup", "exclude_sweeper": "tf test cleanup",
	"tgc_tests": "terraform-google-conversion", "include_in_tgc_next": "terraform-google-conversion",
	"exclude_tgc": "terraform-google-conversion", "cai2hcl_name_format": "cloud asset conversion",
	"cai_asset_name_format": "cloud asset conversion", "cai_base_url": "cloud asset conversion",
	"cai_resource_kind": "cloud asset conversion", "api_resource_type_kind": "cloud asset conversion",
	"api_variant_patterns":    "cloud asset conversion",
	"datasource_experimental": "tf data source", "generate_list_resource": "tf list resource",
	"list_filter": "tf list resource", "filename_override": "tf file layout",
	"deprecation_message": "tf docs", "legacy_name": "tf resource name", "references": "doc links",
	"autogen_status": "mm generator bookkeeping",
	"iam_policy":     "generates separate IAM resources, not this one's lifecycle",
	// Terraform state and identity.
	"timeouts": "tf timeouts; ours come from the catalog", "schema_version": "tf state",
	"state_upgraders": "tf state", "state_upgrade_base_schema_version": "tf state",
	"migrate_state": "tf state", "identity": "tf identity", "identity_schema_version": "tf identity",
	"identity_upgraders": "tf identity", "exclude_identity_generation": "tf identity",
	"exclude_identity_from_identity_import": "tf identity", "exclude_import": "tf import command",
	// Terraform-only behaviour, or Go code under third_party that is MPL 2.0.
	"virtual_fields": "tf-only fields, never sent", "custom_diff": "tf plan-time Go",
	"exclude_default_cdiff": "tf plan-time Go", "bypass_clientside_update_check": "tf plan-time check",
	"taint_resource_on_failed_create": "tf state", "supports_indirect_user_project_override": "tf provider setting",
	"legacy_long_form_project": "tf id spelling", "deletion_policy_custom_docs": "tf deletion_policy field",
	"deletion_policy_default": "tf deletion_policy field", "deletion_policy_exclude": "tf deletion_policy field",
	"exclude_attribution_label": "tf's own provisioning label",
	"error_retry_predicates":    "names Go funcs; our backoff is our own", "error_abort_predicates": "names Go funcs",
	"read_error_transform": "names a Go func",
	// Answered by the Discovery document instead.
	"kind": "the API's kind string", "collection_url_key": "list field, taken from Discovery",
	"list_response_is_array": "list shape, taken from Discovery", "has_self_link": "Discovery says whether selfLink exists",
	"api_resource_field": "cloud asset conversion", "readonly": "data source only; Discovery has no create either",
	"autogen_async": "operation handling is found by shape (AwaitOf)",
}

// ignoredFieldKeys: the same, for a field at any depth.
var ignoredFieldKeys = map[string]string{
	"custom_tgc_expand": "terraform-google-conversion", "custom_tgc_flatten": "terraform-google-conversion",
	"tgc_ignore_read": "terraform-google-conversion", "tgc_ignore_terraform_custom_flatten": "terraform-google-conversion",
	"is_missing_in_cai": "cloud asset conversion", "include_empty_value_in_cai": "cloud asset conversion",
	"exclude_false_in_cai": "cloud asset conversion",
	"diff_suppress_func":   "tf plan-time Go", "state_func": "tf state Go", "set_hash_func": "tf set hashing",
	"key_expander": "tf map key Go", "validation": "tf client-side validation; the API validates",
	"item_validation": "tf client-side validation; the API validates",
	"at_least_one_of": "tf validation; the API validates", "conflicts": "tf validation; the API validates",
	"exactly_one_of": "tf validation; the API validates", "required_with": "tf validation; the API validates",
	"min_size": "tf validation; the API validates", "max_size": "tf validation; the API validates",
	"enum_values": "Discovery carries the enum", "default_value": "tf default; we never send an unset field",
	"allow_empty_object": "tf empty-block handling", "flatten_object": "tf nesting; Discovery's shape is used",
	"key_name": "tf map shape; Discovery's shape is used", "value_type": "tf map shape; Discovery's shape is used",
	"exact_version": "tf docs", "exclude_docs_values": "tf docs", "deprecation_message": "tf docs",
	"removed_message": "tf docs",
}

// yamlKeys lists the yaml tags a struct decodes.
func yamlKeys(v any) map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		if tag := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]; tag != "" && tag != "-" {
			out[tag] = true
		}
	}
	return out
}

// TestEveryKeyInTheCorpusIsReadOrDeliberatelyIgnored walks every resource file
// as raw yaml, fields at every depth including list elements, and fails on a
// key that is neither decoded nor on an ignore list above. It also fails on
// an ignore entry for a key that is now decoded, or that no file uses, so the
// lists cannot rot into a second place to hide keys.
func TestEveryKeyInTheCorpusIsReadOrDeliberatelyIgnored(t *testing.T) {
	root := "../../gen/mmv1/products"
	files, _ := filepath.Glob(filepath.Join(root, "*", "*.yaml"))
	if len(files) < 900 {
		t.Fatalf("found %d files under %s; the corpus is missing, and a guard that walks nothing passes", len(files), root)
	}
	resParsed, fieldParsed := yamlKeys(Resource{}), yamlKeys(Field{})
	resSeen, fieldSeen := map[string]bool{}, map[string]bool{}
	unknownRes, unknownField := map[string]string{}, map[string]string{}

	var walkField func(file string, n *yaml.Node)
	walkFields := func(file string, n *yaml.Node) {
		if n == nil || n.Kind != yaml.SequenceNode {
			return
		}
		for _, f := range n.Content {
			walkField(file, f)
		}
	}
	walkField = func(file string, n *yaml.Node) {
		if n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			fieldSeen[k] = true
			if !fieldParsed[k] && ignoredFieldKeys[k] == "" {
				unknownField[k] = file
			}
			switch k {
			case "properties":
				walkFields(file, v)
			case "item_type":
				walkField(file, v)
			}
		}
	}

	for _, file := range files {
		if filepath.Base(file) == "product.yaml" {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		top := doc.Content[0]
		for i := 0; i+1 < len(top.Content); i += 2 {
			k, v := top.Content[i].Value, top.Content[i+1]
			resSeen[k] = true
			if !resParsed[k] && ignoredResourceKeys[k] == "" {
				unknownRes[k] = file
			}
			if k == "parameters" || k == "properties" {
				walkFields(file, v)
			}
		}
	}

	report := func(what string, m map[string]string) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.Errorf("%s key %q (first seen in %s) is neither decoded nor ignored with a reason", what, k, m[k])
		}
	}
	report("resource", unknownRes)
	report("field", unknownField)
	for k := range ignoredResourceKeys {
		if resParsed[k] || !resSeen[k] {
			t.Errorf("ignoredResourceKeys lists %q, which is decoded or no longer in the corpus", k)
		}
	}
	for k := range ignoredFieldKeys {
		if fieldParsed[k] || !fieldSeen[k] {
			t.Errorf("ignoredFieldKeys lists %q, which is decoded or no longer in the corpus", k)
		}
	}
}
