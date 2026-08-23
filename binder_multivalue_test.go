package binder

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// Repeated form fields used to fail the whole bind with
// "cannot convert []string to slice", even though parseBody produced the
// []string deliberately.
func TestRepeatedFormFieldsBindToSlice(t *testing.T) {
	var got struct {
		Tags []string `body:"tags"`
	}
	r := httptest.NewRequest("POST", "/s", strings.NewReader("tags=a&tags=b&tags=c"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Tags) != 3 || got.Tags[0] != "a" || got.Tags[2] != "c" {
		t.Errorf("Tags = %v, want [a b c]", got.Tags)
	}
}

func TestRepeatedQueryParamsBindToSlice(t *testing.T) {
	var got struct {
		Tags []string `query:"tags"`
	}
	r := httptest.NewRequest("GET", "/s?tags=a&tags=b&tags=c", nil)

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Tags) != 3 || got.Tags[0] != "a" || got.Tags[2] != "c" {
		t.Errorf("Tags = %v, want [a b c]", got.Tags)
	}
}

func TestRepeatedHeadersBindToSlice(t *testing.T) {
	var got struct {
		Accept []string `header:"X-Accept"`
	}
	r := httptest.NewRequest("GET", "/s", nil)
	r.Header.Add("X-Accept", "a")
	r.Header.Add("X-Accept", "b")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Accept) != 2 || got.Accept[0] != "a" || got.Accept[1] != "b" {
		t.Errorf("Accept = %v, want [a b]", got.Accept)
	}
}

// A single value into a slice still yields a one-element slice.
func TestSingleValueIntoSliceUnchanged(t *testing.T) {
	var q struct {
		Tags []string `query:"tags"`
	}
	if err := Bind(httptest.NewRequest("GET", "/s?tags=a", nil), &q); err != nil {
		t.Fatalf("query: got error %v, want nil", err)
	}
	if len(q.Tags) != 1 || q.Tags[0] != "a" {
		t.Errorf("Tags = %v, want [a]", q.Tags)
	}

	var f struct {
		Tags []string `body:"tags"`
	}
	r := httptest.NewRequest("POST", "/s", strings.NewReader("tags=a"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := Bind(r, &f); err != nil {
		t.Fatalf("form: got error %v, want nil", err)
	}
	if len(f.Tags) != 1 || f.Tags[0] != "a" {
		t.Errorf("Tags = %v, want [a]", f.Tags)
	}
}

// A non-slice field keeps taking the first value.
func TestNonSliceFieldTakesFirstValue(t *testing.T) {
	var got struct {
		Tag string `query:"tags"`
	}
	if err := Bind(httptest.NewRequest("GET", "/s?tags=a&tags=b", nil), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Tag != "a" {
		t.Errorf("Tag = %q, want %q", got.Tag, "a")
	}
}

// Elements convert like any other value.
func TestRepeatedValuesConvertElementTypes(t *testing.T) {
	var got struct {
		IDs   []int  `query:"id"`
		Flags []bool `query:"flag"`
	}
	r := httptest.NewRequest("GET", "/s?id=1&id=2&id=3&flag=true&flag=false", nil)

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.IDs) != 3 || got.IDs[0] != 1 || got.IDs[2] != 3 {
		t.Errorf("IDs = %v, want [1 2 3]", got.IDs)
	}
	if len(got.Flags) != 2 || !got.Flags[0] || got.Flags[1] {
		t.Errorf("Flags = %v, want [true false]", got.Flags)
	}
}

// A bad element reports the field, not a bare conversion failure.
func TestBadRepeatedElementReportsField(t *testing.T) {
	var got struct {
		IDs []int `query:"id"`
	}
	err := Bind(httptest.NewRequest("GET", "/s?id=1&id=nope", nil), &got)
	if err == nil {
		t.Fatal("got nil error")
	}

	var bindErr *BindError
	if !errors.As(err, &bindErr) {
		t.Fatalf("errors.As(*BindError) = false for %v", err)
	}
	if bindErr.Field != "IDs" {
		t.Errorf("Field = %q, want %q", bindErr.Field, "IDs")
	}
	if !strings.Contains(err.Error(), "index 1") {
		t.Errorf("error %q does not identify the failing element", err)
	}
}

// A slice field with no values is absent, so required fires and omitempty
// has nothing to skip.
func TestRequiredSliceWithNoValues(t *testing.T) {
	var got struct {
		Tags []string `query:"tags,required"`
	}
	err := Bind(httptest.NewRequest("GET", "/s", nil), &got)
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("got %v, want ErrMissingRequired", err)
	}
}

func TestAbsentSliceLeavesFieldAlone(t *testing.T) {
	var got struct {
		Tags []string `query:"tags"`
	}
	got.Tags = []string{"preexisting"}

	if err := Bind(httptest.NewRequest("GET", "/s", nil), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "preexisting" {
		t.Errorf("Tags = %v, want it untouched", got.Tags)
	}
}

// JSON arrays are unaffected.
func TestJSONArraysUnaffected(t *testing.T) {
	var got struct {
		Tags []string `body:"tags"`
	}
	r := httptest.NewRequest("POST", "/s", strings.NewReader(`{"tags":["a","b"]}`))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "a" || got.Tags[1] != "b" {
		t.Errorf("Tags = %v, want [a b]", got.Tags)
	}
}

// A comma in a single value stays one value: splitting is a convention this
// package does not impose.
func TestCommaInValueIsNotSplit(t *testing.T) {
	var got struct {
		Tags []string `query:"tags"`
	}
	if err := Bind(httptest.NewRequest("GET", "/s?tags=a,b", nil), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "a,b" {
		t.Errorf("Tags = %v, want [\"a,b\"]", got.Tags)
	}
}
