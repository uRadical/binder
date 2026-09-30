package binder

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Embedded structs used to be skipped without a word: a request type sharing
// Paging by embedding it bound nothing into it.

type Paging struct {
	Page  int `query:"page"`
	Limit int `query:"limit,required"`
}

type Audit struct {
	By     string `body:"by"`
	Reason string `body:"reason"`
}

type paging struct {
	Cursor string `query:"cursor"`
}

func embedRequest(target, body string) *http.Request {
	r := httptest.NewRequest("POST", target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestEmbeddedFieldsArePromoted(t *testing.T) {
	var got struct {
		Paging
		*Audit
		paging
		Name string `body:"name"`
	}
	r := embedRequest("/?page=2&limit=5&cursor=c1", `{"name":"n","by":"ada","reason":"typo"}`)
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	if got.Page != 2 || got.Limit != 5 || got.Cursor != "c1" || got.Name != "n" {
		t.Errorf("got %+v", got)
	}
	if got.Audit == nil || got.By != "ada" || got.Reason != "typo" {
		t.Errorf("Audit = %+v, want it allocated and filled", got.Audit)
	}
}

// An embedded pointer is allocated only when one of its fields is sent, so a
// handler can tell whether any of them was.
func TestEmbeddedPointerStaysNilWhenUnused(t *testing.T) {
	var got struct {
		*Audit
		Name string `body:"name"`
	}
	if err := Bind(embedRequest("/", `{"name":"n"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Audit != nil {
		t.Errorf("Audit = %+v, want nil", got.Audit)
	}
}

func TestEmbeddedFieldsFromForm(t *testing.T) {
	var got struct {
		Audit
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader("by=ada&reason=typo"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	if got.By != "ada" || got.Reason != "typo" {
		t.Errorf("got %+v", got)
	}
}

// Failures in a promoted field name it by its path, and required applies.
func TestEmbeddedFieldFailuresNamePath(t *testing.T) {
	var got struct {
		Paging
	}
	err := Bind(httptest.NewRequest("GET", "/?page=x", nil), &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 2 {
		t.Fatalf("got %v, want two failures", err)
	}
	if errs[0].Field != "Paging.Page" || errs[0].Name != "page" {
		t.Errorf("first = %+v, want Paging.Page / page", errs[0])
	}
	if errs[1].Field != "Paging.Limit" || !errors.Is(errs[1], ErrMissingRequired) {
		t.Errorf("second = %+v, want Paging.Limit missing", errs[1])
	}
}

// As with Go's own selectors, an outer field shadows a promoted one with the
// same key.
func TestOuterFieldShadowsEmbedded(t *testing.T) {
	var got struct {
		Audit
		By string `body:"by"`
	}
	if err := Bind(embedRequest("/", `{"by":"outer"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.By != "outer" || got.Audit.By != "" {
		t.Errorf("got outer %q, embedded %q; want outer set, embedded untouched", got.By, got.Audit.By)
	}
}

// An embedded struct with a tag of its own is an ordinary field: a nested
// object, not promoted fields.
func TestTaggedEmbeddedIsNested(t *testing.T) {
	var got struct {
		Audit `body:"audit"`
	}
	if err := Bind(embedRequest("/", `{"audit":{"by":"ada"},"by":"flat"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.By != "ada" {
		t.Errorf("By = %q, want ada from the nested object", got.By)
	}
}

// Promoted body keys are known keys, so DisallowUnknownFields accepts them.
func TestEmbeddedBodyKeysAreKnown(t *testing.T) {
	var got struct {
		Audit
	}
	err := BindWithOptions(embedRequest("/", `{"by":"ada","other":1}`), &got, BindOptions{DisallowUnknownFields: true})
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 1 || errs[0].Name != "other" {
		t.Errorf("got %v, want only other reported", err)
	}
	if got.By != "ada" {
		t.Errorf("By = %q, want ada", got.By)
	}
}

// Embedding works inside a nested struct too.
func TestEmbeddedInsideNestedStruct(t *testing.T) {
	type Line struct {
		*Audit
		Qty int `body:"qty"`
	}
	var got struct {
		Line Line `body:"line"`
	}
	if err := Bind(embedRequest("/", `{"line":{"qty":3,"by":"ada"}}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Line.Qty != 3 || got.Line.Audit == nil || got.Line.By != "ada" {
		t.Errorf("got %+v", got.Line)
	}
}

type cyclic struct {
	*cyclic
	*Cyclic
	V string `body:"v"`
}

type Cyclic struct {
	*cyclic
	W string `body:"w"`
}

// A cycle of embedded pointers is walked once rather than forever.
func TestEmbeddedCycleTerminates(t *testing.T) {
	var got cyclic
	if err := Bind(embedRequest("/", `{"v":"a","w":"b"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.V != "a" || got.Cyclic == nil || got.W != "b" {
		t.Errorf("got V=%q Cyclic=%+v", got.V, got.Cyclic)
	}
	// An unexported embedded pointer cannot be allocated, so it stays nil.
	if got.cyclic != nil {
		t.Errorf("unexported embed allocated: %+v", got.cyclic)
	}
}

// body and json name the same body member, so they are one key: an outer
// body field shadows a promoted json one, and in a flat struct the first
// declared wins, whatever the body's format.
func TestBodyAndJSONTagsShareKeys(t *testing.T) {
	type inner struct {
		Name string `json:"name"`
	}
	var got struct {
		Name string `body:"name"`
		inner
	}
	if err := Bind(embedRequest("/", `{"name":"j"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "j" || got.inner.Name != "" {
		t.Errorf("got outer %q, promoted %q; want outer j", got.Name, got.inner.Name)
	}

	var flat struct {
		A string `body:"x"`
		B string `json:"x"`
	}
	if err := Bind(embedRequest("/", `{"x":"v"}`), &flat); err != nil {
		t.Fatal(err)
	}
	if flat.A != "v" || flat.B != "" {
		t.Errorf("got A=%q B=%q, want A=v", flat.A, flat.B)
	}
}

// As in encoding/json, an embed tagged json:"-" is excluded, and one whose
// tag gives no name is still promoted.
func TestEmbeddedTagRules(t *testing.T) {
	var excluded struct {
		Paging `json:"-"`
		Audit  `json:",omitempty"`
	}
	if err := Bind(embedRequest("/?page=3&limit=1", `{"by":"ada"}`), &excluded); err != nil {
		t.Fatal(err)
	}
	if excluded.Page != 0 || excluded.By != "ada" {
		t.Errorf("got Page=%d By=%q, want Paging excluded and Audit promoted", excluded.Page, excluded.By)
	}
}

type EmbeddedID string

// An embedded type that is not a struct has nothing to promote; with a tag it
// binds as an ordinary field, under its name when the tag gives none.
func TestTaggedNonStructEmbedBinds(t *testing.T) {
	var got struct {
		EmbeddedID `query:",required"`
	}
	if err := Bind(httptest.NewRequest("GET", "/?EmbeddedID=7", nil), &got); err != nil {
		t.Fatal(err)
	}
	if got.EmbeddedID != "7" {
		t.Errorf("EmbeddedID = %q, want 7", got.EmbeddedID)
	}
	err := Bind(httptest.NewRequest("GET", "/", nil), &got)
	if !errors.Is(err, ErrMissingRequired) {
		t.Errorf("got %v, want ErrMissingRequired", err)
	}
}

// A null for a promoted field sets nothing, so its embedded pointer stays nil.
func TestNullDoesNotAllocateEmbeddedPointer(t *testing.T) {
	var got struct {
		*Audit
	}
	if err := Bind(embedRequest("/", `{"by":null}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Audit != nil {
		t.Errorf("Audit = %+v, want nil", got.Audit)
	}
}
