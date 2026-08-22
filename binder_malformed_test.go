package binder

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Malformed JSON must be reported, not silently swallowed into an empty body.
func TestMalformedJSONReturnsError(t *testing.T) {
	var got struct {
		Email string `body:"email"`
	}

	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email": NOT JSON`))
	r.Header.Set("Content-Type", "application/json")

	err := Bind(r, &got)
	if err == nil {
		t.Fatal("malformed JSON body: got nil error, want ErrMalformedBody")
	}
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("errors.Is(err, ErrMalformedBody) = false for %v", err)
	}
	if got.Email != "" {
		t.Errorf("Email = %q, want empty - nothing should bind from an unparseable body", got.Email)
	}
}

// The underlying decoder error stays reachable for callers that want detail.
// Both the sentinel and the decoder failure are wrapped, so the error exposes
// them through the multiple-error Unwrap rather than the single-error one.
func TestMalformedJSONWrapsDecoderError(t *testing.T) {
	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{`))
	r.Header.Set("Content-Type", "application/json")

	var got struct {
		Email string `body:"email"`
	}
	err := Bind(r, &got)
	if err == nil {
		t.Fatal("got nil error, want ErrMalformedBody")
	}

	multi, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("error %T does not wrap multiple errors", err)
	}
	wrapped := multi.Unwrap()
	if len(wrapped) != 2 {
		t.Fatalf("wrapped %d errors, want 2 (the sentinel and the decoder failure)", len(wrapped))
	}
	if !errors.Is(wrapped[0], ErrMalformedBody) {
		t.Errorf("first wrapped error = %v, want ErrMalformedBody", wrapped[0])
	}
	if wrapped[1] == nil || wrapped[1].Error() == "" {
		t.Error("decoder failure was not preserved")
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("error %q does not say the JSON was invalid", err)
	}
}

// A malformed body must not be masked by a misleading required-field error.
func TestMalformedJSONNotReportedAsMissingField(t *testing.T) {
	var got struct {
		Email string `body:"email,required"`
	}

	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email": NOT JSON`))
	r.Header.Set("Content-Type", "application/json")

	err := Bind(r, &got)
	if !errors.Is(err, ErrMalformedBody) {
		t.Fatalf("got %v, want ErrMalformedBody - the parse failure is the real cause", err)
	}
	if strings.Contains(err.Error(), "missing required field") {
		t.Errorf("error %q blames the field when the body itself was unparseable", err)
	}
}

// Valid JSON still binds, and an empty body remains acceptable.
func TestWellFormedBodyStillBinds(t *testing.T) {
	var got struct {
		Email string `body:"email"`
	}
	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email":"a@b.c"}`))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("well-formed body: got error %v, want nil", err)
	}
	if got.Email != "a@b.c" {
		t.Errorf("Email = %q, want %q", got.Email, "a@b.c")
	}
}

func TestEmptyBodyIsNotMalformed(t *testing.T) {
	var got struct {
		ID    string `query:"id"`
		Email string `body:"email"`
	}
	r := httptest.NewRequest("POST", "/u?id=7", nil)
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("empty body: got error %v, want nil", err)
	}
	if got.ID != "7" {
		t.Errorf("ID = %q, want %q", got.ID, "7")
	}
}

// A body binder does not parse cannot be malformed; other sources still bind.
func TestUnparsedContentTypeIsNotMalformed(t *testing.T) {
	var got struct {
		ID    string `query:"id"`
		Email string `body:"email"`
	}
	r := httptest.NewRequest("POST", "/u?id=7", strings.NewReader(`this is not JSON at all`))
	r.Header.Set("Content-Type", "text/plain")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("text/plain body: got error %v, want nil", err)
	}
	if got.ID != "7" {
		t.Errorf("ID = %q, want %q - non-body sources must still bind", got.ID, "7")
	}
}

// The body stays readable by downstream handlers even when parsing fails.
func TestBodyRestoredAfterParseFailure(t *testing.T) {
	const raw = `{"email": NOT JSON`
	r := httptest.NewRequest("POST", "/u", strings.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")

	var got struct {
		Email string `body:"email"`
	}
	if err := Bind(r, &got); !errors.Is(err, ErrMalformedBody) {
		t.Fatalf("got %v, want ErrMalformedBody", err)
	}

	buf := make([]byte, len(raw))
	n, _ := r.Body.Read(buf)
	if string(buf[:n]) != raw {
		t.Errorf("restored body = %q, want %q", buf[:n], raw)
	}
}

var _ = http.MethodPost
