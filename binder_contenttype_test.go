package binder

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// Media types carrying JSON must be parsed as JSON, including the structured
// syntax suffix forms that REST APIs commonly use.
func TestJSONContentTypeVariants(t *testing.T) {
	jsonTypes := []string{
		"application/json",
		"application/json; charset=utf-8",
		"APPLICATION/JSON",
		"text/json",
		"application/vnd.api+json",
		"application/hal+json",
		"application/problem+json",
		"application/ld+json",
		"application/vnd.company.v2+json; charset=utf-8",
	}

	for _, ct := range jsonTypes {
		var got struct {
			Email string `body:"email"`
		}
		r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email":"a@b.c"}`))
		r.Header.Set("Content-Type", ct)

		if err := Bind(r, &got); err != nil {
			t.Errorf("Content-Type %q: got error %v, want nil", ct, err)
			continue
		}
		if got.Email != "a@b.c" {
			t.Errorf("Content-Type %q: Email = %q, want %q", ct, got.Email, "a@b.c")
		}
	}
}

// Types that do not carry JSON must not be parsed as JSON. Multipart is
// absent because it is a body format binder does parse, just not as JSON.
func TestNonJSONContentTypesNotParsed(t *testing.T) {
	nonJSON := []string{
		"text/plain",
		"application/xml",
		"application/octet-stream",
		"application/jsonp",    // not a suffix, a different subtype
		"application/json-rpc", // likewise
		"",
	}

	for _, ct := range nonJSON {
		var got struct {
			ID    string `query:"id"`
			Email string `body:"email"`
		}
		r := httptest.NewRequest("POST", "/u?id=7", strings.NewReader(`{"email":"a@b.c"}`))
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}

		if err := Bind(r, &got); err != nil {
			t.Errorf("Content-Type %q: got error %v, want nil", ct, err)
			continue
		}
		if got.Email != "" {
			t.Errorf("Content-Type %q: body was parsed as JSON, Email = %q", ct, got.Email)
		}
		if got.ID != "7" {
			t.Errorf("Content-Type %q: other sources must still bind, ID = %q", ct, got.ID)
		}
	}
}

// A malformed body under a suffixed JSON type is reported like any other.
func TestMalformedSuffixedJSONReported(t *testing.T) {
	var got struct {
		Email string `body:"email"`
	}
	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email": NOT JSON`))
	r.Header.Set("Content-Type", "application/vnd.api+json")

	if err := Bind(r, &got); !errors.Is(err, ErrMalformedBody) {
		t.Fatalf("got %v, want ErrMalformedBody", err)
	}
}

// A required body field is satisfiable under a suffixed JSON type.
func TestRequiredFieldUnderSuffixedJSON(t *testing.T) {
	var got struct {
		Email string `body:"email,required"`
	}
	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email":"a@b.c"}`))
	r.Header.Set("Content-Type", "application/hal+json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
}

// Form encoding is unaffected.
func TestFormContentTypeUnaffected(t *testing.T) {
	var got struct {
		Email string `body:"email"`
	}
	r := httptest.NewRequest("POST", "/u", strings.NewReader("email=a%40b.c"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Email != "a@b.c" {
		t.Errorf("Email = %q, want %q", got.Email, "a@b.c")
	}
}

func TestIsJSONContentType(t *testing.T) {
	tests := []struct {
		ct   string
		want bool
	}{
		{"application/json", true},
		{"text/json", true},
		{"application/vnd.api+json", true},
		{"application/problem+json", true},
		{"application/xml", false},
		{"application/jsonp", false},
		{"application/x-www-form-urlencoded", false},
		{"json", false}, // no subtype at all
		{"", false},
		{"application/+json", true}, // degenerate but still a JSON suffix
	}
	for _, tt := range tests {
		if got := isJSONContentType(tt.ct); got != tt.want {
			t.Errorf("isJSONContentType(%q) = %v, want %v", tt.ct, got, tt.want)
		}
	}
}
