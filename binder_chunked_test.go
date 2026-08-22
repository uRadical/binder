package binder

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// unknownLengthReader hides the length of its content, so net/http cannot
// infer a Content-Length and sends the request chunked.
type unknownLengthReader struct{ io.Reader }

// serveChunked sends body to a handler over a real connection with chunked
// transfer encoding, and returns whatever the handler recorded.
func serveChunked(t *testing.T, contentType, body string, handle func(*http.Request)) {
	t.Helper()

	var sawChunked bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawChunked = r.ContentLength == -1 && slices.Contains(r.TransferEncoding, "chunked")
		handle(r)
	}))
	defer srv.Close()

	req, err := http.NewRequest("POST", srv.URL, unknownLengthReader{strings.NewReader(body)})
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.ContentLength = -1
	req.Header.Set("Content-Type", contentType)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("sending request: %v", err)
	}
	resp.Body.Close()

	if !sawChunked {
		t.Fatal("request did not arrive chunked, so this test proves nothing")
	}
}

// A chunked JSON body declares no length. It must still be bound.
func TestChunkedJSONBodyBinds(t *testing.T) {
	var got struct {
		Email string   `body:"email"`
		Tags  []string `body:"tags"`
	}
	var bindErr error

	serveChunked(t, "application/json", `{"email":"a@b.c","tags":["x","y"]}`, func(r *http.Request) {
		bindErr = Bind(r, &got)
	})

	if bindErr != nil {
		t.Fatalf("chunked JSON body: got error %v, want nil", bindErr)
	}
	if got.Email != "a@b.c" {
		t.Errorf("Email = %q, want %q", got.Email, "a@b.c")
	}
	if len(got.Tags) != 2 {
		t.Errorf("Tags = %v, want two entries", got.Tags)
	}
}

func TestChunkedFormBodyBinds(t *testing.T) {
	form := url.Values{"email": {"a@b.c"}}.Encode()

	var got struct {
		Email string `body:"email"`
	}
	var bindErr error

	serveChunked(t, "application/x-www-form-urlencoded", form, func(r *http.Request) {
		bindErr = Bind(r, &got)
	})

	if bindErr != nil {
		t.Fatalf("chunked form body: got error %v, want nil", bindErr)
	}
	if got.Email != "a@b.c" {
		t.Errorf("Email = %q, want %q", got.Email, "a@b.c")
	}
}

// A required body field must be satisfiable by a chunked body.
func TestChunkedBodySatisfiesRequired(t *testing.T) {
	var got struct {
		Email string `body:"email,required"`
	}
	var bindErr error

	serveChunked(t, "application/json", `{"email":"a@b.c"}`, func(r *http.Request) {
		bindErr = Bind(r, &got)
	})

	if bindErr != nil {
		t.Fatalf("chunked body with required field: got error %v, want nil", bindErr)
	}
}

// The size cap still applies when no length is declared - this is the case
// the Content-Length check could never have caught.
func TestChunkedBodyStillCapped(t *testing.T) {
	withMaxBodySize(t, 1024)

	var got struct {
		Data string `body:"data"`
	}
	var bindErr error

	serveChunked(t, "application/json", jsonBodyOfSize(64<<10), func(r *http.Request) {
		bindErr = Bind(r, &got)
	})

	if !errors.Is(bindErr, ErrBodyTooLarge) {
		t.Fatalf("oversized chunked body: got %v, want ErrBodyTooLarge", bindErr)
	}
}

// Malformed JSON arriving chunked is still reported.
func TestChunkedMalformedBodyReported(t *testing.T) {
	var got struct {
		Email string `body:"email"`
	}
	var bindErr error

	serveChunked(t, "application/json", `{"email": NOT JSON`, func(r *http.Request) {
		bindErr = Bind(r, &got)
	})

	if !errors.Is(bindErr, ErrMalformedBody) {
		t.Fatalf("malformed chunked body: got %v, want ErrMalformedBody", bindErr)
	}
}

// An empty body must not be mistaken for a malformed one now that
// Content-Length no longer gates the read.
func TestEmptyChunkedBodyIsNotMalformed(t *testing.T) {
	var got struct {
		Email string `body:"email"`
	}
	var bindErr error

	serveChunked(t, "application/json", "", func(r *http.Request) {
		bindErr = Bind(r, &got)
	})

	if bindErr != nil {
		t.Fatalf("empty chunked body: got error %v, want nil", bindErr)
	}
}

// A zero-length body with a declared length stays acceptable too.
func TestZeroLengthBodyIsNotMalformed(t *testing.T) {
	var got struct {
		ID    string `query:"id"`
		Email string `body:"email"`
	}

	r := httptest.NewRequest("POST", "/u?id=7", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("zero-length body: got error %v, want nil", err)
	}
	if got.ID != "7" {
		t.Errorf("ID = %q, want %q", got.ID, "7")
	}
}
