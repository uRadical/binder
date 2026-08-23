package binder

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHeaderBinds(t *testing.T) {
	var got struct {
		Auth  string `header:"Authorization"`
		Trace string `header:"X-Request-ID"`
	}

	r := httptest.NewRequest("GET", "/s", nil)
	r.Header.Set("Authorization", "Bearer t0k")
	r.Header.Set("X-Request-ID", "abc-123")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Auth != "Bearer t0k" {
		t.Errorf("Auth = %q, want %q", got.Auth, "Bearer t0k")
	}
	if got.Trace != "abc-123" {
		t.Errorf("Trace = %q, want %q", got.Trace, "abc-123")
	}
}

// HTTP header names are case-insensitive, so the tag may spell one however it
// likes and still match the header the client sent. net/http canonicalises
// incoming header keys, and Header.Get canonicalises the key it looks up, so
// the two meet regardless of how either was written.
func TestHeaderNameIsCaseInsensitive(t *testing.T) {
	newReq := func() *http.Request {
		r := httptest.NewRequest("GET", "/s", nil)
		r.Header.Set("X-Request-ID", "abc-123")
		return r
	}

	// One struct per tag spelling, since a tag is fixed at compile time.
	var canonical struct {
		Trace string `header:"X-Request-Id"`
	}
	var upper struct {
		Trace string `header:"X-REQUEST-ID"`
	}
	var lower struct {
		Trace string `header:"x-request-id"`
	}
	var mixed struct {
		Trace string `header:"x-ReQuEsT-iD"`
	}

	targets := []struct {
		spelling string
		trace    *string
		target   interface{}
	}{
		{"X-Request-Id", &canonical.Trace, &canonical},
		{"X-REQUEST-ID", &upper.Trace, &upper},
		{"x-request-id", &lower.Trace, &lower},
		{"x-ReQuEsT-iD", &mixed.Trace, &mixed},
	}

	for _, tt := range targets {
		if err := Bind(newReq(), tt.target); err != nil {
			t.Fatalf("tag %q: got error %v, want nil", tt.spelling, err)
		}
		if *tt.trace != "abc-123" {
			t.Errorf("tag %q did not match the header: got %q, want %q", tt.spelling, *tt.trace, "abc-123")
		}
	}
}

// A lowercase tag must match a canonically-sent header.
func TestLowercaseHeaderTagMatches(t *testing.T) {
	var got struct {
		Trace string `header:"x-request-id"`
	}
	r := httptest.NewRequest("GET", "/s", nil)
	r.Header.Set("X-Request-ID", "abc-123")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Trace != "abc-123" {
		t.Errorf("Trace = %q, want %q", got.Trace, "abc-123")
	}
}

func TestMissingHeaderLeavesFieldAlone(t *testing.T) {
	var got struct {
		Trace string `header:"X-Request-ID"`
	}
	got.Trace = "preexisting"

	if err := Bind(httptest.NewRequest("GET", "/s", nil), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Trace != "preexisting" {
		t.Errorf("Trace = %q, want it untouched", got.Trace)
	}
}

func TestRequiredHeader(t *testing.T) {
	var got struct {
		Auth string `header:"Authorization,required"`
	}

	err := Bind(httptest.NewRequest("GET", "/s", nil), &got)
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("missing required header: got %v, want ErrMissingRequired", err)
	}

	var bindErr *BindError
	if !errors.As(err, &bindErr) {
		t.Fatalf("errors.As(*BindError) = false for %v", err)
	}
	if bindErr.Source != header || bindErr.Name != "Authorization" {
		t.Errorf("BindError = %+v, want Source=header Name=Authorization", bindErr)
	}
}

// Headers convert like any other source.
func TestHeaderConvertsTypes(t *testing.T) {
	var got struct {
		Count int       `header:"X-Count"`
		Ratio float64   `header:"X-Ratio"`
		Debug bool      `header:"X-Debug"`
		Since time.Time `header:"X-Since"`
	}

	r := httptest.NewRequest("GET", "/s", nil)
	r.Header.Set("X-Count", "42")
	r.Header.Set("X-Ratio", "0.5")
	r.Header.Set("X-Debug", "true")
	r.Header.Set("X-Since", "2026-01-02T03:04:05Z")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Count != 42 || got.Ratio != 0.5 || !got.Debug {
		t.Errorf("bound %+v, want Count=42 Ratio=0.5 Debug=true", got)
	}
	if !got.Since.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("Since = %v, want 2026-01-02T03:04:05Z", got.Since)
	}
}

// Headers sit alongside the other sources without disturbing them.
func TestHeaderAlongsideOtherSources(t *testing.T) {
	type request struct {
		ID    string `path:"id"`
		Q     string `query:"q"`
		Email string `body:"email"`
		Token string `cookie:"token"`
		Trace string `header:"X-Request-ID"`
	}

	mux := http.NewServeMux()
	var got request
	var bindErr error
	mux.HandleFunc("POST /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		bindErr = Bind(r, &got)
	})

	r := httptest.NewRequest("POST", "/users/42?q=find", strings.NewReader(`{"email":"a@b.c"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Request-ID", "abc-123")
	r.AddCookie(&http.Cookie{Name: "token", Value: "t0k"})
	mux.ServeHTTP(httptest.NewRecorder(), r)

	if bindErr != nil {
		t.Fatalf("got error %v, want nil", bindErr)
	}
	want := request{ID: "42", Q: "find", Email: "a@b.c", Token: "t0k", Trace: "abc-123"}
	if got != want {
		t.Errorf("bound %+v, want %+v", got, want)
	}
}

// Header is last in precedence, so a field carrying both binds from the other.
func TestHeaderIsLastInPrecedence(t *testing.T) {
	var got struct {
		V string `query:"v" header:"X-V"`
	}

	r := httptest.NewRequest("GET", "/s?v=from-query", nil)
	r.Header.Set("X-V", "from-header")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.V != "from-query" {
		t.Errorf("V = %q, want %q - query outranks header", got.V, "from-query")
	}
}

// Only the first value of a repeated header binds, as with query parameters.
func TestRepeatedHeaderTakesFirst(t *testing.T) {
	var got struct {
		V string `header:"X-Multi"`
	}
	r := httptest.NewRequest("GET", "/s", nil)
	r.Header.Add("X-Multi", "first")
	r.Header.Add("X-Multi", "second")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.V != "first" {
		t.Errorf("V = %q, want %q", got.V, "first")
	}
}

// Content-Type is an ordinary header as far as binding is concerned.
func TestContentTypeHeaderBindable(t *testing.T) {
	var got struct {
		CT    string `header:"Content-Type"`
		Email string `body:"email"`
	}
	r := httptest.NewRequest("POST", "/s", strings.NewReader(`{"email":"a@b.c"}`))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.CT != "application/json" || got.Email != "a@b.c" {
		t.Errorf("bound %+v, want the header and the body both", got)
	}
}
