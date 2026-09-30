package binder

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func nestedReq(t *testing.T, body string) *strings.Reader {
	t.Helper()
	return strings.NewReader(body)
}

// A tagged unexported field inside a nested struct cannot be set, and used to
// panic on the attempt. Top-level fields were guarded; nested ones were not.
func TestNestedUnexportedFieldIgnored(t *testing.T) {
	type inner struct {
		Ok     string `body:"ok"`
		hidden string `body:"hidden"` //lint:ignore U1000 fixture for the unexported-field case
	}
	var got struct {
		N inner `body:"n"`
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Bind panicked on a nested unexported field: %v", p)
		}
	}()

	r := httptest.NewRequest("POST", "/u", nestedReq(t, `{"n":{"ok":"a","hidden":"b"}}`))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.N.Ok != "a" {
		t.Errorf("Ok = %q, want %q - exported siblings must still bind", got.N.Ok, "a")
	}
	if got.N.hidden != "" {
		t.Errorf("hidden = %q, want empty", got.N.hidden)
	}
}

// Deeply nested structs bind through the same one implementation.
func TestDeeplyNestedBinding(t *testing.T) {
	type level3 struct {
		Value string `body:"value"`
	}
	type level2 struct {
		L3  level3  `body:"l3"`
		Ptr *level3 `body:"ptr"`
	}
	type level1 struct {
		L2 level2 `body:"l2"`
	}
	var got struct {
		L1 level1 `body:"l1"`
	}

	body := `{"l1":{"l2":{"l3":{"value":"deep"},"ptr":{"value":"pointer"}}}}`
	r := httptest.NewRequest("POST", "/u", nestedReq(t, body))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.L1.L2.L3.Value != "deep" {
		t.Errorf("L3.Value = %q, want %q", got.L1.L2.L3.Value, "deep")
	}
	if got.L1.L2.Ptr == nil || got.L1.L2.Ptr.Value != "pointer" {
		t.Errorf("Ptr = %+v, want a pointer holding \"pointer\"", got.L1.L2.Ptr)
	}
}

// An error from deep in the tree names the field it came from.
func TestNestedBindingErrorNamesField(t *testing.T) {
	type inner struct {
		N int `body:"n"`
	}
	var got struct {
		I inner `body:"i"`
	}

	r := httptest.NewRequest("POST", "/u", nestedReq(t, `{"i":{"n":"not a number"}}`))
	r.Header.Set("Content-Type", "application/json")

	err := Bind(r, &got)
	if err == nil {
		t.Fatal("got nil error")
	}
	if !strings.Contains(err.Error(), "field I.N") {
		t.Errorf("error %q does not name the nested field", err)
	}
}

// A struct field handed something that is not an object is refused.
func TestStructFieldFromNonObject(t *testing.T) {
	type inner struct {
		N int `body:"n"`
	}
	var got struct {
		I inner `body:"i"`
	}

	r := httptest.NewRequest("POST", "/u", nestedReq(t, `{"i":"a string"}`))
	r.Header.Set("Content-Type", "application/json")

	err := Bind(r, &got)
	if err == nil {
		t.Fatal("got nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "cannot set struct field") {
		t.Errorf("error %q does not explain the mismatch", err)
	}
}

// Untagged nested fields are left alone.
func TestNestedUntaggedFieldIgnored(t *testing.T) {
	type inner struct {
		Tagged   string `body:"tagged"`
		Untagged string
	}
	var got struct {
		N inner `body:"n"`
	}
	got.N.Untagged = "preexisting"

	r := httptest.NewRequest("POST", "/u", nestedReq(t, `{"n":{"tagged":"a","Untagged":"b"}}`))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.N.Tagged != "a" {
		t.Errorf("Tagged = %q, want %q", got.N.Tagged, "a")
	}
	if got.N.Untagged != "preexisting" {
		t.Errorf("Untagged = %q, want it untouched", got.N.Untagged)
	}
}

// required and omitempty used to be ignored inside a nested struct.
func TestNestedTagOptions(t *testing.T) {
	type Address struct {
		City string `body:"city,omitempty"`
		Zip  string `body:"zip,required"`
	}
	var got struct {
		Address Address `body:"address"`
	}
	got.Address.City = "preset"
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"address":{"city":""}}`))
	r.Header.Set("Content-Type", "application/json")
	err := Bind(r, &got)

	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 1 || !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("got %v, want one ErrMissingRequired", err)
	}
	if errs[0].Field != "Address.Zip" || errs[0].Name != "address.zip" {
		t.Errorf("got Field=%q Name=%q, want Address.Zip, address.zip", errs[0].Field, errs[0].Name)
	}
	if got.Address.City != "preset" {
		t.Errorf("City = %q, want the preset kept", got.Address.City)
	}
}

// A JSON null left a pointer allocated at its zero value on the map-based
// build, so null looked the same as 0. It leaves the pointer nil on both.
func TestNullLeavesPointerNil(t *testing.T) {
	type Address struct {
		City string `body:"city"`
	}
	var got struct {
		Retries *int     `body:"retries"`
		Address *Address `body:"address"`
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"retries":null,"address":null}`))
	r.Header.Set("Content-Type", "application/json")
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	if got.Retries != nil || got.Address != nil {
		t.Errorf("got Retries=%v Address=%v, want both nil", got.Retries, got.Address)
	}
}
