package binder

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// Every query field must bind, however many there are.
func TestManyQueryParametersAllBind(t *testing.T) {
	var got struct {
		A string `query:"a"`
		B string `query:"b"`
		C string `query:"c"`
		D string `query:"d"`
		E string `query:"e"`
	}

	r := httptest.NewRequest("GET", "/s?a=1&b=2&c=3&d=4&e=5", nil)
	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.A != "1" || got.B != "2" || got.C != "3" || got.D != "4" || got.E != "5" {
		t.Errorf("bound %+v, want a..e = 1..5", got)
	}
}

// Repeated parameters keep net/url's first-value semantics.
func TestRepeatedQueryParameterTakesFirst(t *testing.T) {
	var got struct {
		A string `query:"a"`
	}
	r := httptest.NewRequest("GET", "/s?a=first&a=second", nil)

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.A != "first" {
		t.Errorf("A = %q, want %q", got.A, "first")
	}
}

// A missing parameter is absent, not empty, and an absent one does not
// disturb the parameters around it.
func TestMissingQueryParameterAmongPresentOnes(t *testing.T) {
	var got struct {
		A string `query:"a"`
		B string `query:"b"`
		C string `query:"c"`
	}
	got.B = "preexisting"

	r := httptest.NewRequest("GET", "/s?a=1&c=3", nil)
	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.A != "1" || got.C != "3" {
		t.Errorf("bound %+v, want a=1 c=3", got)
	}
	if got.B != "preexisting" {
		t.Errorf("B = %q, want it untouched", got.B)
	}
}

func TestEmptyQueryString(t *testing.T) {
	var got struct {
		A string `query:"a,required"`
	}
	r := httptest.NewRequest("GET", "/s", nil)

	if err := Bind(r, &got); err == nil {
		t.Fatal("required query parameter with no query string: got nil error")
	}
}

// Values are percent-decoded once, not once per field.
func TestQueryValuesAreDecodedCorrectly(t *testing.T) {
	var got struct {
		A string `query:"a"`
		B string `query:"b"`
	}
	r := httptest.NewRequest("GET", "/s?a=hello%20world&b=%2Bplus%26amp", nil)

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.A != "hello world" {
		t.Errorf("A = %q, want %q", got.A, "hello world")
	}
	if got.B != "+plus&amp" {
		t.Errorf("B = %q, want %q", got.B, "+plus&amp")
	}
}

// Query parsing must not disturb binding from the other sources.
func TestQueryAlongsideOtherSources(t *testing.T) {
	var got struct {
		Q     string `query:"q"`
		Email string `body:"email"`
	}
	r := httptest.NewRequest("POST", "/s?q=find", strings.NewReader(`{"email":"a@b.c"}`))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Q != "find" || got.Email != "a@b.c" {
		t.Errorf("bound %+v, want q=find email=a@b.c", got)
	}
}

// A short query is scanned per field and never parsed into url.Values; a long
// one is parsed once, when a field first asks, and reused.
func TestQueryScannedOrParsedOnce(t *testing.T) {
	short := queryCache{url: httptest.NewRequest("GET", "/s?a=1&b=2", nil).URL}
	if got := short.get("b"); got != "2" {
		t.Errorf("short get(b) = %q, want %q", got, "2")
	}
	if short.parsed != nil {
		t.Error("short query was parsed into url.Values")
	}

	pairs := make([]string, maxScanPairs+1)
	for i := range pairs {
		pairs[i] = fmt.Sprintf("k%d=%d", i, i)
	}
	long := queryCache{url: httptest.NewRequest("GET", "/s?"+strings.Join(pairs, "&"), nil).URL}
	if long.parsed != nil {
		t.Fatal("long query parsed before any field asked for it")
	}
	if got := long.get("k64"); got != "64" {
		t.Errorf("long get(k64) = %q, want %q", got, "64")
	}
	parsed := long.parsed
	if parsed == nil {
		t.Fatal("long query not parsed on first use")
	}
	long.get("k1")
	if reflect.ValueOf(long.parsed).Pointer() != reflect.ValueOf(parsed).Pointer() {
		t.Error("long query parsed again rather than reused")
	}
}

// queryScanAgrees checks that scanning raw for each key gives what
// url.ParseQuery gives: the same first value and the same list of values.
func queryScanAgrees(t *testing.T, raw string) {
	t.Helper()
	u := &url.URL{RawQuery: raw}
	want := u.Query()
	q := queryCache{url: u}

	// Every key the standard library found, plus a few it may have dropped.
	keys := []string{"a", "b", "a b", "a+b", "", ";", "%", "k"}
	for k := range want {
		keys = append(keys, k)
	}
	for _, k := range keys {
		if got, w := q.get(k), want.Get(k); got != w {
			t.Errorf("RawQuery %q: get(%q) = %q, url.Values says %q", raw, k, got, w)
		}
		if got, w := q.all(k), want[k]; !reflect.DeepEqual(got, w) {
			t.Errorf("RawQuery %q: all(%q) = %q, url.Values says %q", raw, k, got, w)
		}
	}
}

// The scan must skip exactly what url.ParseQuery skips and unescape exactly
// as it does.
func TestQueryScanMatchesParseQuery(t *testing.T) {
	for _, raw := range []string{
		"",
		"a=1",
		"a=1&a=2&b=3",
		"a=&a=2",          // an empty first value
		"a",               // no equals sign
		"a=1=2",           // equals inside the value
		"&&a=1&&",         // empty pairs
		"a=1;b=2&b=3",     // semicolon: the whole pair is dropped
		"a=%zz&a=2",       // bad escape in the value: skipped, the next is first
		"%zz=1&a=2",       // bad escape in the key
		"a%20b=1&a+b=2",   // escaped keys
		"a=x%20y&a=x+y",   // escaped values
		"a=%E2%9C%93",     // UTF-8
		"a=%",             // truncated escape
		"b=1&a=2&b=3&a=4", // interleaved
	} {
		queryScanAgrees(t, raw)
	}
}

func FuzzQueryScan(f *testing.F) {
	for _, seed := range []string{"a=1&a=2", "a=%zz&a=2", "a;b=1", "a+b=%20", "&=&a"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		queryScanAgrees(t, raw)
	})
}
