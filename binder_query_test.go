package binder

import (
	"net/http/httptest"
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

// A target with no query tag must not pay to parse the query string.
func TestQueryNotParsedWhenUnused(t *testing.T) {
	q := queryCache{url: httptest.NewRequest("GET", "/s?a=1", nil).URL}
	if q.values != nil {
		t.Fatal("query parsed before any field asked for it")
	}
	if got := q.get("a"); got != "1" {
		t.Errorf("get(a) = %q, want %q", got, "1")
	}
	if q.values == nil {
		t.Error("query not retained after first use")
	}
}
