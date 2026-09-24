package gen

import (
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
	"github.com/infrena/infrena/pkg/value"
)

// setterFixture is a compute-shaped global proxy: read under /global/, its
// setUrlMap published without it, its setLabels naming the id {resource},
// and two methods that must be refused -- one whose request carries a field
// magic-modules does not group under it, one with no request body at all
// (compute instance's setDeletionProtection takes its value as a query
// parameter), and a third whose path names a placeholder the resource's own
// address does not have.
func setterFixture() (*disco.Document, disco.Collection, *mmv1.Resource, *catalog.Type) {
	str := &disco.Schema{Type: "string"}
	doc := &disco.Document{Name: "compute", Schemas: map[string]*disco.Schema{
		"UrlMapReference":  {ID: "UrlMapReference", Type: "object", Properties: map[string]*disco.Schema{"urlMap": str}},
		"SetLabelsRequest": {ID: "SetLabelsRequest", Type: "object", Properties: map[string]*disco.Schema{"labels": {Type: "object"}, "labelFingerprint": str}},
		"BackendReference": {ID: "BackendReference", Type: "object", Properties: map[string]*disco.Schema{"service": str}},
		"SetQuicRequest":   {ID: "SetQuicRequest", Type: "object", Properties: map[string]*disco.Schema{"quicOverride": str, "reason": str}},
	}}
	base := "projects/{project}/global/proxies/{proxy}"
	col := disco.Collection{Methods: map[string]*disco.Method{
		"get":                   {Path: base, HTTPMethod: "GET"},
		"setUrlMap":             {Path: "projects/{project}/proxies/{proxy}/setUrlMap", HTTPMethod: "POST", Request: &disco.Ref{Ref: "UrlMapReference"}},
		"setLabels":             {Path: "projects/{project}/global/proxies/{resource}/setLabels", HTTPMethod: "POST", Request: &disco.Ref{Ref: "SetLabelsRequest"}},
		"setQuicOverride":       {Path: base + "/setQuicOverride", HTTPMethod: "POST", Request: &disco.Ref{Ref: "SetQuicRequest"}},
		"setDeletionProtection": {Path: base + "/setDeletionProtection", HTTPMethod: "POST"},
		"setBackendService":     {Path: "projects/{project}/backends/{backend}/setBackendService", HTTPMethod: "POST", Request: &disco.Ref{Ref: "BackendReference"}},
	}}
	mm := &mmv1.Resource{Name: "Proxy", Immutable: true, Properties: []*mmv1.Field{
		{Name: "urlMap", UpdateURL: "projects/{{project}}/proxies/{{name}}/setUrlMap", UpdateVerb: "POST"},
		{Name: "certificateManagerCertificates", UpdateURL: "projects/{{project}}/proxies/{{name}}/setUrlMap", UpdateVerb: "POST"},
		{Name: "labels", UpdateURL: "projects/{{project}}/global/proxies/{{name}}/setLabels", UpdateVerb: "POST"},
		{Name: "labelFingerprint", UpdateURL: "projects/{{project}}/global/proxies/{{name}}/setLabels", UpdateVerb: "POST"},
		{Name: "quicOverride", UpdateURL: "projects/{{project}}/global/proxies/{{name}}/setQuicOverride", UpdateVerb: "POST"},
		{Name: "deletionProtection", UpdateURL: "projects/{{project}}/global/proxies/{{name}}/setDeletionProtection", UpdateVerb: "POST"},
		{Name: "service", UpdateURL: "projects/{{project}}/backends/{{name}}/setBackendService", UpdateVerb: "POST"},
		{Name: "securityPolicy", UpdateURL: "projects/{{project}}/global/proxies/{{name}}", UpdateVerb: "PATCH"},
	}}
	ty := &catalog.Type{Name: "gcp.proxy", SelfLink: base, Attributes: map[string]*catalog.Attr{
		"name":               {Canonical: "name", Kind: value.KindString, Required: true},
		"description":        {Canonical: "description", Kind: value.KindString},
		"urlMap":             {Canonical: "urlMap", Kind: value.KindString, ForceNew: true},
		"labels":             {Canonical: "labels", Kind: value.KindMap, Output: true},
		"labelFingerprint":   {Canonical: "labelFingerprint", Kind: value.KindString, Output: true},
		"quicOverride":       {Canonical: "quicOverride", Kind: value.KindString, ForceNew: true},
		"deletionProtection": {Canonical: "deletionProtection", Kind: value.KindBool, ForceNew: true},
		"service":            {Canonical: "service", Kind: value.KindString, ForceNew: true},
	}}
	return doc, col, mm, ty
}

// TestASetterIsAdmittedOnlyWhereDiscoveryAgrees.
func TestASetterIsAdmittedOnlyWhereDiscoveryAgrees(t *testing.T) {
	doc, col, mm, ty := setterFixture()
	got := map[string]catalog.Setter{}
	for _, s := range discoveredSetters(doc, col, mm, ty) {
		got[s.Method] = s
	}

	if s, ok := got["setUrlMap"]; !ok {
		t.Error("setUrlMap was refused; its path drops /global/, as compute's does, and every placeholder is the resource's own")
	} else {
		if s.Path != "projects/{project}/proxies/{proxy}/setUrlMap" {
			t.Errorf("setUrlMap path = %q, want Discovery's own path", s.Path)
		}
		if len(s.Fields) != 1 || s.Fields[0] != "urlMap" {
			t.Errorf("setUrlMap fields = %v, want [urlMap]: certificateManagerCertificates is Terraform's name, not the API's", s.Fields)
		}
	}
	if s, ok := got["setLabels"]; !ok {
		t.Error("setLabels was refused")
	} else {
		if s.Path != "projects/{project}/global/proxies/{proxy}/setLabels" {
			t.Errorf("setLabels path = %q, want {resource} renamed to the self_link's {proxy}", s.Path)
		}
		if s.Lock != "labelFingerprint" || len(s.Fields) != 1 || s.Fields[0] != "labels" {
			t.Errorf("setLabels fields %v lock %q, want [labels] locked on labelFingerprint", s.Fields, s.Lock)
		}
	}
	if _, ok := got["setQuicOverride"]; ok {
		t.Error("setQuicOverride was admitted, but its request carries a field nothing would send")
	}
	if _, ok := got["setBackendService"]; ok {
		t.Error("setBackendService was admitted, but its path names {backend}, which nothing about this resource supplies")
	}
	if _, ok := got["setDeletionProtection"]; ok {
		t.Error("setDeletionProtection was admitted, but it has no request body to carry the field")
	}
	if len(got) != 2 {
		t.Errorf("setters = %v, want exactly setUrlMap and setLabels", got)
	}
}

// TestASetterFieldIsSettableAndTheRestReplace. A type with no update verb
// becomes updatable through its setters, so every field they do not carry
// must replace it, or the host would call Update for a change nothing sends.
func TestASetterFieldIsSettableAndTheRestReplace(t *testing.T) {
	doc, col, mm, ty := setterFixture()
	ty.Setters = discoveredSetters(doc, col, mm, ty)
	applySetters(ty, mm)

	if a := ty.Attributes["urlMap"]; a.ForceNew {
		t.Error("urlMap is still ForceNew, but setUrlMap changes it in place")
	}
	if a := ty.Attributes["labels"]; a.Output || a.ForceNew {
		t.Errorf("labels = %+v, want settable: Discovery's output-only means the insert ignores it, which is why setLabels exists", a)
	}
	if !ty.Attributes["labelFingerprint"].Output {
		t.Error("the lock became settable")
	}
	if !ty.Attributes["description"].ForceNew {
		t.Error("description is not ForceNew on a type whose only updates are setters that do not carry it")
	}
	if !ty.Attributes["quicOverride"].ForceNew {
		t.Error("quicOverride lost ForceNew although its setter was refused")
	}
}
