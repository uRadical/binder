package binder

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"
)

// These targets cover the shapes added after the first fuzz targets were
// written: bracketed query maps, embedded structs, maps, any, types that
// decode themselves from JSON, and durations.

// FuzzQueryGroup holds the name[key]=value scan to url.ParseQuery, as
// FuzzQueryScan does for single values: it must skip and unescape exactly
// what the standard library does, whether it scans the raw query or, past
// maxScanPairs, reads the parsed url.Values.
func FuzzQueryGroup(f *testing.F) {
	for _, seed := range []struct{ raw, name string }{
		{"filter[a]=1&filter[b]=2&filter[a]=3", "filter"},
		{"filter%5Ba%5D=1&filter[a]=", "filter"},
		{"f[a][b]=1&f[]=2&f=3&f[a=4&fa]=5", "f"},
		{"m[%zz]=1&m[k]=%zz&m[k];x=1", "m"},
		{"a+b[c+d]=x+y", "a b"},
	} {
		f.Add(seed.raw, seed.name)
	}
	f.Fuzz(func(t *testing.T, raw, name string) {
		u := &url.URL{RawQuery: raw}
		values, _ := url.ParseQuery(raw)

		var want map[string]any
		for key, vs := range values {
			if sub, ok := bracketKey(key, name); ok {
				for _, v := range vs {
					want = addGrouped(want, sub, v)
				}
			}
		}

		scanned := queryCache{url: u, checked: true, short: true}
		if got := scanned.group(name); !reflect.DeepEqual(got, want) {
			t.Errorf("scan of %q for %q = %v, url.ParseQuery gives %v", raw, name, got, want)
		}
		parsed := queryCache{url: u, checked: true, short: false}
		if got := parsed.group(name); !reflect.DeepEqual(got, want) {
			t.Errorf("parsed %q for %q = %v, url.ParseQuery gives %v", raw, name, got, want)
		}
	})
}

// FuzzBracketKey holds bracketKey to its definition: key is name, "[", a
// non-empty sub-key containing no brackets, and "]".
func FuzzBracketKey(f *testing.F) {
	for _, seed := range [][2]string{{"filter[a]", "filter"}, {"filter[]", "filter"}, {"f[a][b]", "f"}, {"[a]", ""}, {"fa]", "f"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, key, name string) {
		got, ok := bracketKey(key, name)

		rest, hasPrefix := strings.CutPrefix(key, name+"[")
		sub, hasSuffix := strings.CutSuffix(rest, "]")
		wantOK := hasPrefix && hasSuffix && sub != "" && !strings.ContainsAny(sub, "[]")
		if ok != wantOK || (ok && got != sub) {
			t.Errorf("bracketKey(%q, %q) = %q, %v; want %q, %v", key, name, got, ok, sub, wantOK)
		}
	})
}

// The JSON differential target: a struct whose shapes binder and json/v2
// should decode identically, given json tags and string-typed values so that
// neither side's conversions come into it.

type fuzzInner struct {
	City string `json:"city"`
}

type fuzzShared struct {
	Page  string `json:"page"`
	Owner string `json:"owner"`
}

type fuzzDeep struct {
	Deep string `json:"deep"`
}

type fuzzMiddle struct {
	fuzzDeep
	Mid string `json:"mid"`
}

type fuzzEmbedDoc struct {
	fuzzShared
	fuzzMiddle
	Owner string            `json:"owner"` // shadows fuzzShared.Owner
	Name  string            `json:"name"`
	Addr  fuzzInner         `json:"addr"`
	Meta  map[string]string `json:"meta"`
	Tags  []string          `json:"tags"`
	Raw   json.RawMessage   `json:"raw"`
}

// FuzzEmbeddedMatchesJSON decodes the same body with binder and with json/v2.
// Where json/v2 accepts it, binder must too, and bind the same values: the
// promotion and shadowing of embedded fields, maps, slices, nested structs
// and a RawMessage handed over as sent.
func FuzzEmbeddedMatchesJSON(f *testing.F) {
	f.Add(`{"page":"2","owner":"outer","deep":"d","mid":"m","name":"n"}`)
	f.Add(`{"addr":{"city":"x"},"meta":{"a":"b","c":"d"},"tags":["t",null],"raw":{ "k" : [1, 2] }}`)
	f.Add(`{"name":null,"meta":{},"raw":null}`)
	f.Add(`{"n\u0061me":"escaped","extra":{"ignored":true}}`)
	f.Add(`null`)

	f.Fuzz(func(t *testing.T, body string) {
		// json/v2 refuses duplicate member names by default, and a body with
		// them is left out: binder applies each occurrence in turn, so a later
		// null sets nothing and a later object replaces a map, where json/v2
		// resets and merges.
		var want fuzzEmbedDoc
		if err := jsonv2.Unmarshal([]byte(body), &want); err != nil {
			return // outside what the two are expected to agree on
		}

		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		var got fuzzEmbedDoc
		if err := Bind(r, &got); err != nil {
			t.Fatalf("json/v2 accepts %q, but Bind fails: %v", body, err)
		}
		// A null sets nothing in binder, for every field alike; json/v2 keeps a
		// RawMessage's null as the bytes "null". That difference is by design.
		if got.Raw == nil && string(want.Raw) == "null" {
			want.Raw = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("body %q:\nBind    %#v\njson/v2 %#v", body, got, want)
		}
	})
}

// fuzzCents decodes a JSON number itself, as a money type would.
type fuzzCents int64

func (c *fuzzCents) UnmarshalJSON(b []byte) error {
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*c = fuzzCents(f * 100)
	return nil
}

type fuzzEmbedPtr struct {
	By string `body:"by"`
	At int    `query:"at"`
}

type fuzzShapes struct {
	*fuzzEmbedPtr
	Paging

	Meta    map[string]string        `body:"meta"`
	Counts  map[int]int              `body:"counts"`
	Filter  map[string]bool          `query:"f"`
	Waits   map[string]time.Duration `query:"w"`
	Teams   map[uuid.UUID]string     `body:"teams"`
	Any     any                      `body:"any"`
	Price   fuzzCents                `body:"price"`
	Prices  []fuzzCents              `body:"prices"`
	Raw     json.RawMessage          `body:"raw"`
	RawQ    json.RawMessage          `query:"rq"`
	Timeout time.Duration            `body:"timeout"`
	Retry   *time.Duration           `header:"X-Retry"`
	Opt     *bool                    `body:"opt,omitempty"`
}

// FuzzBindShapes drives the newer shapes from every source. The contract is
// the one FuzzBind holds, that Bind returns and leaves the body readable, and
// one more: every field failure names the input it came from. The one entry
// whose name may be empty is an unknown member whose name was itself empty.
func FuzzBindShapes(f *testing.F) {
	f.Add("application/json", `{"by":"x","meta":{"a":"b"},"counts":{"1":2},"any":[1,{"a":null}],"price":1.5,"prices":[1,"2"],"raw":{"a":1},"timeout":"5s","opt":false}`, "page=2&at=3&f[x]=true&w[a]=1s&rq=v", "10s")
	f.Add("application/x-www-form-urlencoded", "meta[a]=b&counts[1]=x&price=2.5&timeout=1m&by=", "f[a]=maybe&w[b]=5", "soon")
	f.Add("application/json", `{"teams":{"not-a-uuid":"x"},"counts":{"k":1},"meta":"flat"}`, "limit=x&f[]=1&f[a][b]=1", "")
	f.Add("application/json", `{"by":null,"meta":{"a":null},"any":null}`, "", "")

	f.Fuzz(func(t *testing.T, contentType, body, query, header string) {
		if !isHeaderSafe(contentType) || !isHeaderSafe(header) {
			t.Skip()
		}
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		r.URL.RawQuery = query
		if header != "" {
			r.Header.Set("X-Retry", header)
		}

		var got fuzzShapes
		err := BindWithOptions(r, &got, BindOptions{DisallowUnknownFields: len(body)%2 == 0})

		var errs BindErrors
		if errors.As(err, &errs) {
			for _, e := range errs {
				if e.Name == "" && !errors.Is(e, ErrUnknownField) {
					t.Errorf("failure without a name: %+v", e)
				}
			}
		}
		if _, readErr := io.ReadAll(r.Body); readErr != nil {
			t.Fatalf("body unreadable after Bind: %v", readErr)
		}
	})
}
