package binder

import (
	"errors"
	"net/http/httptest"
	"reflect"
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
	if !strings.Contains(err.Error(), "nested field N") {
		t.Errorf("error %q does not name the nested field", err)
	}
}

// BindStruct is exported for manual nested binding; both a struct value and a
// pointer to one are accepted.
func TestBindStructAcceptsValueAndPointer(t *testing.T) {
	type target struct {
		A string `body:"a"`
		B string `json:"b"`
	}
	data := map[string]interface{}{"a": "one", "b": "two"}

	var direct target
	if err := BindStruct(reflect.ValueOf(&direct).Elem(), data); err != nil {
		t.Fatalf("struct value: got error %v, want nil", err)
	}
	if direct.A != "one" || direct.B != "two" {
		t.Errorf("bound %+v, want both fields set", direct)
	}

	var viaPtr *target
	field := reflect.ValueOf(&viaPtr).Elem()
	if err := BindStruct(field, data); err != nil {
		t.Fatalf("nil pointer: got error %v, want nil", err)
	}
	if viaPtr == nil {
		t.Fatal("nil pointer was not allocated")
	}
	if viaPtr.A != "one" {
		t.Errorf("A = %q, want %q", viaPtr.A, "one")
	}
}

// BindStruct given something that is not a struct reports it rather than
// panicking inside reflect.
func TestBindStructRejectsNonStruct(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("BindStruct panicked: %v", p)
		}
	}()

	var n int
	err := BindStruct(reflect.ValueOf(&n).Elem(), map[string]interface{}{"a": "1"})
	if !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("got %v, want ErrInvalidTarget", err)
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
