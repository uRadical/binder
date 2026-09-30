package binder

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type mapItem struct {
	Qty int `body:"qty"`
}

// Maps used to be refused outright as "unsupported type: map". A JSON object
// binds into one, converting keys and values to the map's types.
func TestMapsFromJSONObjects(t *testing.T) {
	var got struct {
		Meta   map[string]string         `body:"meta"`
		Counts map[string]int            `body:"counts"`
		Any    map[string]any            `body:"any"`
		Items  map[string]mapItem        `body:"items"`
		Lists  map[string][]string       `body:"lists"`
		ByID   map[int]string            `body:"by_id"`
		Ptrs   map[string]*mapItem       `body:"ptrs"`
		Nested map[string]map[string]int `body:"nested"`
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{
		"meta": {"a": "b", "c": ""},
		"counts": {"x": 1, "y": "2"},
		"any": {"n": 1, "s": "t", "o": {"k": true}},
		"items": {"sku1": {"qty": 3}},
		"lists": {"tags": ["p", "q"]},
		"by_id": {"7": "seven"},
		"ptrs": {"p": {"qty": 1}, "nil": null},
		"nested": {"a": {"b": 2}}
	}`))
	r.Header.Set("Content-Type", "application/json")
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"Meta", got.Meta, map[string]string{"a": "b", "c": ""}},
		{"Counts", got.Counts, map[string]int{"x": 1, "y": 2}},
		{"Items", got.Items, map[string]mapItem{"sku1": {Qty: 3}}},
		{"Lists", got.Lists, map[string][]string{"tags": {"p", "q"}}},
		{"ByID", got.ByID, map[int]string{7: "seven"}},
		{"Nested", got.Nested, map[string]map[string]int{"a": {"b": 2}}},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if got.Any["n"] != json.Number("1") || got.Any["s"] != "t" || got.Any["o"].(map[string]any)["k"] != true {
		t.Errorf("Any = %v", got.Any)
	}
	if got.Ptrs["p"] == nil || got.Ptrs["p"].Qty != 1 || got.Ptrs["nil"] != nil {
		t.Errorf("Ptrs = %v", got.Ptrs)
	}
}

// A bad entry is reported by key, every one of them, in key order, and the
// field is left as it was.
func TestMapFailuresNamedByKey(t *testing.T) {
	var got struct {
		Counts map[string]int `body:"counts"`
		ByID   map[int]string `body:"by_id"`
	}
	got.Counts = map[string]int{"keep": 1}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"counts":{"b":"x","a":"y","ok":1},"by_id":{"seven":"7"}}`))
	r.Header.Set("Content-Type", "application/json")
	err := Bind(r, &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 3 {
		t.Fatalf("got %v, want three failures", err)
	}
	want := [][2]string{{`Counts["a"]`, "counts[a]"}, {`Counts["b"]`, "counts[b]"}, {`ByID["seven"]`, "by_id[seven]"}}
	for i, w := range want {
		if errs[i].Field != w[0] || errs[i].Name != w[1] {
			t.Errorf("entry %d = %s / %s, want %s / %s", i, errs[i].Field, errs[i].Name, w[0], w[1])
		}
	}
	if !reflect.DeepEqual(got.Counts, map[string]int{"keep": 1}) {
		t.Errorf("Counts = %v, want it left as it was", got.Counts)
	}
}

// A value that is not an object is an ordinary conversion error.
func TestMapFromNonObject(t *testing.T) {
	var got struct {
		Meta map[string]string `body:"meta"`
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"meta":"flat"}`))
	r.Header.Set("Content-Type", "application/json")
	var errs BindErrors
	if err := Bind(r, &got); !errors.As(err, &errs) || errs[0].Name != "meta" {
		t.Errorf("got %v, want a failure for meta", err)
	}
}

// A field of type any takes the decoded value as encoding/json would, with
// numbers kept as json.Number.
func TestAnyFieldTakesDecodedValue(t *testing.T) {
	var got struct {
		Payload any `body:"payload"`
		Count   any `body:"count"`
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"payload":{"a":[1,"b"]},"count":9007199254740993}`))
	r.Header.Set("Content-Type", "application/json")
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": []any{json.Number("1"), "b"}}
	if !reflect.DeepEqual(got.Payload, want) || got.Count != json.Number("9007199254740993") {
		t.Errorf("got Payload=%#v Count=%#v", got.Payload, got.Count)
	}
}

// A query spells a map as name[key]=value pairs, OpenAPI's deepObject style.
// Keys and values convert as they do from a JSON object.
func TestMapsFromQuery(t *testing.T) {
	var got struct {
		Filter map[string]string   `query:"filter"`
		Min    map[string]int      `query:"min"`
		Flags  map[string]bool     `query:"flag"`
		Tags   map[string][]string `query:"tags"`
		ByID   map[int]string      `query:"name"`
		Opt    map[string]*int     `query:"opt"`
		Absent map[string]string   `query:"absent"`
	}
	q := "/?filter[status]=open&filter%5Bteam%5D=core&min[price]=10&flag[draft]=0&flag[live]=true" +
		"&tags[any]=a&tags[any]=b&name[7]=seven&opt[limit]=0&filter[empty]=&filter[a][b]=nested&filter=flat"
	if err := Bind(httptest.NewRequest("GET", q, nil), &got); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"Filter", got.Filter, map[string]string{"status": "open", "team": "core"}},
		{"Min", got.Min, map[string]int{"price": 10}},
		{"Flags", got.Flags, map[string]bool{"draft": false, "live": true}},
		{"Tags", got.Tags, map[string][]string{"any": {"a", "b"}}},
		{"ByID", got.ByID, map[int]string{7: "seven"}},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if got.Opt["limit"] == nil || *got.Opt["limit"] != 0 {
		t.Errorf("Opt = %v, want limit pointing at 0", got.Opt)
	}
	if got.Absent != nil {
		t.Errorf("Absent = %v, want nil", got.Absent)
	}
}

// A bad entry is reported under the name the client sent, and required means
// at least one entry.
func TestQueryMapFailures(t *testing.T) {
	var got struct {
		Min    map[string]int    `query:"min"`
		Filter map[string]string `query:"filter,required"`
	}
	err := Bind(httptest.NewRequest("GET", "/?min[price]=cheap&min[qty]=2&filter[x]=", nil), &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 2 {
		t.Fatalf("got %v, want two failures", err)
	}
	if errs[0].Name != "min[price]" || errs[0].Source != "query" || errs[1].Name != "filter" || !errors.Is(errs[1], ErrMissingRequired) {
		t.Errorf("got %v / %v", errs[0], errs[1])
	}
}

// A form body uses the same encoding, and its bracketed fields are known
// fields for DisallowUnknownFields.
func TestMapsFromForm(t *testing.T) {
	for _, ct := range []string{"application/x-www-form-urlencoded", "multipart/form-data"} {
		t.Run(ct, func(t *testing.T) {
			var got struct {
				Meta map[string]string `body:"meta"`
				Qty  map[string]int    `body:"qty"`
			}
			r := formRequest(t, ct, "meta[a]", "b", "qty[x]", "3", "qty[x]", "4")
			if err := BindWithOptions(r, &got, BindOptions{DisallowUnknownFields: true}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Meta, map[string]string{"a": "b"}) || got.Qty["x"] != 3 {
				t.Errorf("got Meta=%v Qty=%v", got.Meta, got.Qty)
			}
		})
	}
}

// Past 64 parameters the query is parsed once instead of scanned; maps are
// gathered the same way.
func TestQueryMapFromLongQuery(t *testing.T) {
	var got struct {
		Filter map[string]int `query:"filter"`
	}
	var b strings.Builder
	b.WriteString("/?filter[a]=1")
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&b, "&pad%d=x", i)
	}
	b.WriteString("&filter[b]=2")
	if err := Bind(httptest.NewRequest("GET", b.String(), nil), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Filter, map[string]int{"a": 1, "b": 2}) {
		t.Errorf("Filter = %v", got.Filter)
	}
}
