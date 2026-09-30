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
}
