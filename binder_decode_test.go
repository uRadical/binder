package binder

import (
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Arrays, objects and nested structs are decoded token by token into their Go
// types. These pin the rules that path shares with the rest of binding.

func bindJSONBody(t *testing.T, body string, target any) error {
	t.Helper()
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return Bind(r, target)
}

func TestRawJSONEmpty(t *testing.T) {
	for raw, want := range map[string]bool{
		`null`: true, `false`: true, `true`: false,
		`""`: true, `" "`: false, `"x"`: false,
		`0`: true, `0.0`: true, `-0`: true, `1`: false, `1e-9`: false,
		`{}`: true, `{ }`: true, "[\n]": true, `[0]`: false, `{"a":1}`: false,
	} {
		if got := rawJSONEmpty(jsontext.Value(raw)); got != want {
			t.Errorf("rawJSONEmpty(%s) = %v, want %v", raw, got, want)
		}
	}
}

// omitempty on an array, object or struct keeps the field as it was when the
// value is empty, and binds it otherwise, at the top level and nested.
func TestOmitEmptyOnComposites(t *testing.T) {
	type inner struct {
		Tags []string        `body:"tags,omitempty"`
		Meta map[string]int  `body:"meta,omitempty"`
		Sub  struct{ A int } `body:"sub,omitempty"`
	}
	var got struct {
		Tags  []string       `body:"tags,omitempty"`
		Meta  map[string]int `body:"meta,omitempty"`
		Inner inner          `body:"inner"`
	}
	got.Tags, got.Meta = []string{"keep"}, map[string]int{"keep": 1}
	got.Inner.Tags = []string{"keep"}
	if err := bindJSONBody(t, `{"tags":[],"meta":{ },"inner":{"tags":[],"meta":{"a":2}}}`, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Tags, []string{"keep"}) || got.Meta["keep"] != 1 || !reflect.DeepEqual(got.Inner.Tags, []string{"keep"}) || got.Inner.Meta["a"] != 2 {
		t.Errorf("got %+v", got)
	}
	if err := bindJSONBody(t, `{"tags":["x"]}`, &got); err != nil || !reflect.DeepEqual(got.Tags, []string{"x"}) {
		t.Errorf("got %v, Tags=%v; want [x]", err, got.Tags)
	}
}

// Every bad element is reported under its index, and the slice is left as it
// was. A body cut-off inside an array is malformed.
func TestArrayElementFailures(t *testing.T) {
	var got struct {
		N []int `body:"n"`
	}
	got.N = []int{7}
	err := bindJSONBody(t, `{"n":[1,"x",3,1.5]}`, &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 2 || errs[0].Name != "n[1]" || errs[1].Name != "n[3]" {
		t.Fatalf("got %v, want failures for n[1] and n[3]", err)
	}
	if !reflect.DeepEqual(got.N, []int{7}) {
		t.Errorf("N = %v, want it left as it was", got.N)
	}
	if err := bindJSONBody(t, `{"n":[1,`, &got); !errors.Is(err, ErrMalformedBody) {
		t.Errorf("truncated: got %v, want ErrMalformedBody", err)
	}
	if err := bindJSONBody(t, `{"n":[1 2]}`, &got); !errors.Is(err, ErrMalformedBody) {
		t.Errorf("missing comma: got %v, want ErrMalformedBody", err)
	}
}

// Map keys convert to the key type, a null value gives its key the zero
// value, and every failure is named by key.
func TestObjectIntoMapFailures(t *testing.T) {
	var got struct {
		ByID  map[int]string  `body:"by_id"`
		Count map[string]*int `body:"count"`
	}
	if err := bindJSONBody(t, `{"by_id":{"7":"x"},"count":{"a":null,"b":2}}`, &got); err != nil {
		t.Fatal(err)
	}
	if got.ByID[7] != "x" || got.Count["a"] != nil || *got.Count["b"] != 2 {
		t.Errorf("got %+v", got)
	}
	err := bindJSONBody(t, `{"by_id":{"seven":"x","8":{}}}`, &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 2 || errs[0].Name != "by_id[8]" || errs[1].Name != "by_id[seven]" {
		t.Errorf("got %v, want failures for by_id[8] and by_id[seven]", err)
	}
}

// Elements that decode themselves, or from text, do so inside a slice or map
// as they do at the top level.
func TestCustomElementTypes(t *testing.T) {
	var got struct {
		Times  []time.Time           `body:"times"`
		Prices []cents               `body:"prices"`
		ByDay  map[string]*time.Time `body:"by_day"`
		Waits  []time.Duration       `body:"waits"`
	}
	body := `{"times":["2026-01-02T00:00:00Z"],"prices":[1.25],"by_day":{"mon":"2026-01-05T00:00:00Z"},"waits":["1s",5]}`
	if err := bindJSONBody(t, body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Times) != 1 || got.Times[0].Day() != 2 || got.Prices[0] != 125 || got.ByDay["mon"].Day() != 5 || got.Waits[0] != time.Second || got.Waits[1] != 5 {
		t.Errorf("got %+v", got)
	}
}

// omitempty decides emptiness as the value is read, whatever the field's type:
// an empty object or array leaves the field unset, including a RawMessage. On
// a pointer omitempty has no effect, so {} still gives a pointer to a zero
// struct.
func TestOmitEmptyDecidedWhileReading(t *testing.T) {
	type inner struct {
		A int `json:"a"`
	}
	var got struct {
		P   *inner          `json:"p,omitempty"`
		Q   *inner          `json:"q,omitempty"`
		Raw json.RawMessage `json:"raw,omitempty"`
		N   int             `json:"n,omitempty"`
		Any any             `json:"any,omitempty"`
	}
	if err := bindJSONBody(t, `{"p":{},"q":{"a":2},"raw":[],"any":[1]}`, &got); err != nil {
		t.Fatal(err)
	}
	if got.P == nil || got.P.A != 0 || got.Q == nil || got.Q.A != 2 || len(got.Raw) != 0 || !reflect.DeepEqual(got.Any, []any{json.Number("1")}) {
		t.Errorf("got %+v", got)
	}
	var errs BindErrors
	if err := bindJSONBody(t, `{"n":[1]}`, &got); !errors.As(err, &errs) || errs[0].Name != "n" {
		t.Errorf("array into int: got %v", err)
	}
}
