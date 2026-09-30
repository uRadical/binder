package binder

import (
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// cents decodes a JSON number of pounds, such as 12.5, into whole pence.
type cents int64

func (c *cents) UnmarshalJSON(b []byte) error {
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return fmt.Errorf("price must be a number: %w", err)
	}
	*c = cents(f*100 + 0.5)
	return nil
}

// level decodes itself with json/v2's streaming interface.
type level int

func (l *level) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	switch tok.String() {
	case "low":
		*l = 1
	case "high":
		*l = 2
	default:
		return fmt.Errorf("unknown level %q", tok.String())
	}
	return nil
}

// both has UnmarshalJSON and UnmarshalText, and records which ran.
type both struct{ via string }

func (b *both) UnmarshalJSON([]byte) error { b.via = "json"; return nil }
func (b *both) UnmarshalText([]byte) error { b.via = "text"; return nil }

// Types with their own JSON decoding used to be refused, as "cannot set
// struct field with value of type json.Number" and the like. They decode
// themselves, at the top level and nested, and json.RawMessage keeps its bytes.
func TestTypesDecodeThemselvesFromJSON(t *testing.T) {
	type line struct {
		Price cents `body:"price"`
	}
	var got struct {
		Price  cents            `body:"price"`
		PtrP   *cents           `body:"ptr_price"`
		Level  level            `body:"level"`
		Raw    json.RawMessage  `body:"raw"`
		Line   line             `body:"line"`
		Prices []cents          `body:"prices"`
		ByKey  map[string]cents `body:"by_key"`
		Null   *cents           `body:"null_price"`
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{
		"price": 12.5, "ptr_price": 1, "level": "high",
		"raw": {"z": 1, "a": [true]},
		"line": {"price": 2.25}, "prices": [0.1, 0.2], "by_key": {"a": 3},
		"null_price": null
	}`))
	r.Header.Set("Content-Type", "application/json")
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	if got.Price != 1250 || got.PtrP == nil || *got.PtrP != 100 || got.Level != 2 {
		t.Errorf("got Price=%d PtrP=%v Level=%d", got.Price, got.PtrP, got.Level)
	}
	if string(got.Raw) != `{"z": 1, "a": [true]}` {
		t.Errorf("Raw = %s, want the bytes as sent", got.Raw)
	}
	if got.Line.Price != 225 || !reflect.DeepEqual(got.Prices, []cents{10, 20}) || got.ByKey["a"] != 300 {
		t.Errorf("got Line=%v Prices=%v ByKey=%v", got.Line, got.Prices, got.ByKey)
	}
	if got.Null != nil {
		t.Errorf("Null = %v, want nil", *got.Null)
	}
}

// A type's own error is reported against the field, with the rest bound.
func TestTypeDecodeErrorNamesField(t *testing.T) {
	var got struct {
		Price cents  `body:"price"`
		Level level  `body:"level"`
		Name  string `body:"name"`
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"price":"lots","level":"mid","name":"n"}`))
	r.Header.Set("Content-Type", "application/json")
	err := Bind(r, &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 2 || errs[0].Name != "price" || errs[1].Name != "level" {
		t.Fatalf("got %v, want failures for price and level", err)
	}
	if !strings.Contains(errs[0].Error(), "price must be a number") {
		t.Errorf("error %q does not carry the type's own message", errs[0])
	}
	if got.Name != "n" {
		t.Errorf("Name = %q, want the other fields bound", got.Name)
	}
}

// A string goes to UnmarshalText when the type has both, from any source, so
// a value reads the same from a body as from a query string; anything else
// goes to UnmarshalJSON.
func TestStringPrefersUnmarshalText(t *testing.T) {
	var got struct {
		S both `body:"s"`
		N both `body:"n"`
		Q both `query:"q"`
	}
	r := httptest.NewRequest("POST", "/?q=x", strings.NewReader(`{"s":"x","n":1}`))
	r.Header.Set("Content-Type", "application/json")
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	if got.S.via != "text" || got.N.via != "json" || got.Q.via != "text" {
		t.Errorf("got S=%s N=%s Q=%s, want text, json, text", got.S.via, got.N.via, got.Q.via)
	}
}

// A type with only UnmarshalJSON can still be given a value from a query
// string or form: it receives the text as a JSON string.
func TestJSONOnlyTypeFromTextSource(t *testing.T) {
	var got struct {
		Level level `query:"level"`
	}
	if err := Bind(httptest.NewRequest("GET", "/?level=low", nil), &got); err != nil {
		t.Fatal(err)
	}
	if got.Level != 1 {
		t.Errorf("Level = %d, want 1", got.Level)
	}
}
