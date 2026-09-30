package binder

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func jsonReq(t *testing.T, bodyJSON string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/u?empty=hello", strings.NewReader(bodyJSON))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// Options from two different tags must not combine into one. This field binds
// from body under the name "omit" and has no options at all, but concatenating
// its tags spells "omitempty" across the pair.
func TestOmitEmptyNotSpelledAcrossTwoTags(t *testing.T) {
	var got struct {
		V string `body:"omit" json:"empty"`
	}
	got.V = "preexisting"

	if err := Bind(jsonReq(t, `{"omit":""}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.V != "" {
		t.Errorf(`V = %q, want "" - "omit"+"empty" is not the omitempty option`, got.V)
	}
}

// A parameter merely named "omitempty" does not carry the option.
func TestParameterNamedOmitEmptyIsNotTheOption(t *testing.T) {
	var got struct {
		V string `body:"omitempty"`
	}
	got.V = "untouched"

	if err := Bind(jsonReq(t, `{"omitempty":""}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.V != "" {
		t.Errorf(`V = %q, want "" - the field is named omitempty, it does not opt out`, got.V)
	}
}

// Only the tag the field actually binds from contributes options. This field
// binds from body, so an omitempty spelled on the unused json tag must not
// apply to it.
func TestOmitEmptyOnlyReadFromTheBindingTag(t *testing.T) {
	var got struct {
		V string `body:"a" json:"b,omitempty"`
	}
	got.V = "preexisting"

	if err := Bind(jsonReq(t, `{"a":""}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.V != "" {
		t.Errorf("V = %q, want %q - body is the binding source and carries no options", got.V, "")
	}
}

// The option still works where it is genuinely spelled.
func TestOmitEmptySkipsEmptyValue(t *testing.T) {
	var got struct {
		Email string `body:"email,omitempty"`
	}
	got.Email = "preexisting"

	if err := Bind(jsonReq(t, `{"email":""}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Email != "preexisting" {
		t.Errorf("Email = %q, want the value left untouched", got.Email)
	}
}

func TestOmitEmptyBindsNonEmptyValue(t *testing.T) {
	var got struct {
		Email string `body:"email,omitempty"`
	}
	got.Email = "preexisting"

	if err := Bind(jsonReq(t, `{"email":"a@b.c"}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Email != "a@b.c" {
		t.Errorf("Email = %q, want %q", got.Email, "a@b.c")
	}
}

// Without the option an empty value overwrites, as before.
func TestWithoutOmitEmptyEmptyValueBinds(t *testing.T) {
	var got struct {
		Email string `body:"email"`
	}
	got.Email = "preexisting"

	if err := Bind(jsonReq(t, `{"email":""}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Email != "" {
		t.Errorf("Email = %q, want it overwritten with the empty value", got.Email)
	}
}

// omitempty combined with another option is still recognised.
func TestOmitEmptyAlongsideRequired(t *testing.T) {
	var got struct {
		Email string `body:"email,required,omitempty"`
	}
	got.Email = "preexisting"

	if err := Bind(jsonReq(t, `{"email":""}`), &got); err != nil {
		t.Fatalf("got error %v, want nil - the key is present, so required is satisfied", err)
	}
	if got.Email != "preexisting" {
		t.Errorf("Email = %q, want the value left untouched by omitempty", got.Email)
	}
}

// omitempty used to skip a JSON false or 0 even on a pointer field, so a PATCH
// setting a flag to false was silently lost. On a pointer it has no effect:
// absent stays nil, and a zero value is set.
func TestOmitEmptyIgnoredOnPointers(t *testing.T) {
	type Inner struct {
		On *bool `body:"on,omitempty"`
	}
	var got struct {
		Active *bool   `body:"active,omitempty"`
		Count  *int    `body:"count,omitempty"`
		Name   *string `body:"name,omitempty"`
		Absent *bool   `body:"absent,omitempty"`
		Page   *int    `query:"page,omitempty"`
		Inner  Inner   `body:"inner"`
	}
	r := httptest.NewRequest("POST", "/?page=0", strings.NewReader(`{"active":false,"count":0,"name":"","inner":{"on":false}}`))
	r.Header.Set("Content-Type", "application/json")
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	if got.Active == nil || *got.Active || got.Count == nil || *got.Count != 0 || got.Name == nil || *got.Name != "" {
		t.Errorf("got Active=%v Count=%v Name=%v, want each set to its zero value", got.Active, got.Count, got.Name)
	}
	if got.Page == nil || *got.Page != 0 || got.Inner.On == nil || *got.Inner.On {
		t.Errorf("got Page=%v Inner.On=%v, want both set", got.Page, got.Inner.On)
	}
	if got.Absent != nil {
		t.Errorf("Absent = %v, want nil", *got.Absent)
	}
}
