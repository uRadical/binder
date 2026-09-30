package binder

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

type sizedRequest struct {
	Data string `body:"data"`
}

// jsonBodyOfSize builds a JSON body whose encoded length is exactly size bytes.
func jsonBodyOfSize(size int) string {
	const envelope = `{"data":""}`
	return `{"data":"` + strings.Repeat("x", size-len(envelope)) + `"}`
}

// bindWithLimit binds with a per-call body size limit.
func bindWithLimit(r *http.Request, target any, limit int64) error {
	return BindWithOptions(r, target, BindOptions{MaxBodySize: limit})
}

func TestBodyWithinLimitBinds(t *testing.T) {
	body := jsonBodyOfSize(1024)
	r := httptest.NewRequest("POST", "/u", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	var got sizedRequest
	if err := bindWithLimit(r, &got, 1024); err != nil {
		t.Fatalf("body exactly at the limit: got error %v, want nil", err)
	}
	if len(got.Data) != 1024-len(`{"data":""}`) {
		t.Errorf("Data length = %d, want %d", len(got.Data), 1024-len(`{"data":""}`))
	}
}

func TestBodyOverLimitRejected(t *testing.T) {
	r := httptest.NewRequest("POST", "/u", strings.NewReader(jsonBodyOfSize(1025)))
	r.Header.Set("Content-Type", "application/json")

	var got sizedRequest
	err := bindWithLimit(r, &got, 1024)
	if err == nil {
		t.Fatal("body one byte over the limit: got nil error, want ErrBodyTooLarge")
	}
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("errors.Is(err, ErrBodyTooLarge) = false for %v", err)
	}
	if got.Data != "" {
		t.Error("field was bound from a rejected body")
	}
}

// A client that understates Content-Length must not slip past the limit: the
// cap has to be enforced while reading, not from the declared length.
func TestUnderstatedContentLengthStillRejected(t *testing.T) {
	r := httptest.NewRequest("POST", "/u", strings.NewReader(jsonBodyOfSize(64<<10)))
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = 32 // a lie

	var got sizedRequest
	err := bindWithLimit(r, &got, 1024)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("understated Content-Length: got %v, want ErrBodyTooLarge", err)
	}
}

// An honestly declared oversized body is refused without being read.
func TestOversizedContentLengthNotRead(t *testing.T) {
	tripwire := &trackingReader{Reader: strings.NewReader(jsonBodyOfSize(4096))}
	r := httptest.NewRequest("POST", "/u", tripwire)
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = 4096

	var got sizedRequest
	if err := bindWithLimit(r, &got, 1024); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("declared oversized body: got %v, want ErrBodyTooLarge", err)
	}
	if tripwire.reads != 0 {
		t.Errorf("body was read %d times, want 0 - an oversized body should be refused unread", tripwire.reads)
	}
}

// A negative limit restores unbounded reading.
func TestNegativeLimitDisablesCap(t *testing.T) {
	r := httptest.NewRequest("POST", "/u", strings.NewReader(jsonBodyOfSize(256<<10)))
	r.Header.Set("Content-Type", "application/json")

	var got sizedRequest
	if err := bindWithLimit(r, &got, -1); err != nil {
		t.Fatalf("limit disabled: got error %v, want nil", err)
	}
	if len(got.Data) == 0 {
		t.Error("body was not bound with the limit disabled")
	}
}

// Bind, and BindWithOptions with no limit given, apply DefaultMaxBodySize, so
// setting only another option cannot silently remove the cap.
func TestDefaultMaxBodySize(t *testing.T) {
	if DefaultMaxBodySize != 10<<20 {
		t.Errorf("DefaultMaxBodySize = %d, want 10 MB", DefaultMaxBodySize)
	}

	binds := map[string]func(*http.Request, any) error{
		"Bind": Bind,
		"BindWithOptions zero limit": func(r *http.Request, target any) error {
			return BindWithOptions(r, target, BindOptions{DisallowUnknownFields: true})
		},
	}
	for name, bind := range binds {
		t.Run(name, func(t *testing.T) {
			for size, wantErr := range map[int]bool{
				int(DefaultMaxBodySize):     false,
				int(DefaultMaxBodySize) + 1: true,
			} {
				r := httptest.NewRequest("POST", "/u", strings.NewReader(jsonBodyOfSize(size)))
				r.Header.Set("Content-Type", "application/json")
				var got sizedRequest
				err := bind(r, &got)
				if gotErr := errors.Is(err, ErrBodyTooLarge); gotErr != wantErr {
					t.Errorf("%d byte body: got %v, want ErrBodyTooLarge %v", size, err, wantErr)
				}
			}
		})
	}
}

type trackingReader struct {
	io.Reader
	reads int
}

func (t *trackingReader) Read(p []byte) (int, error) {
	t.reads++
	return t.Reader.Read(p)
}

// readBody reads into one buffer sized from Content-Length where it can. These
// pin its edges: a length that overstates the body, reads that trickle in a
// byte at a time past the initial buffer, data that arrives with io.EOF, and
// a failure part way through.
func TestReadBodyEdges(t *testing.T) {
	bind := func(body io.Reader, contentLength, limit int64) (sizedRequest, error) {
		r := httptest.NewRequest("POST", "/u", body)
		r.Header.Set("Content-Type", "application/json")
		r.ContentLength = contentLength
		var got sizedRequest
		err := bindWithLimit(r, &got, limit)
		return got, err
	}
	large := jsonBodyOfSize(5000) // well past the 512 byte initial buffer

	t.Run("overstated length", func(t *testing.T) {
		got, err := bind(strings.NewReader(`{"data":"x"}`), 1000, 1024)
		if err != nil || got.Data != "x" {
			t.Fatalf("got %+v, %v; want the short body bound", got, err)
		}
	})

	t.Run("byte at a time, no declared length", func(t *testing.T) {
		got, err := bind(iotest.OneByteReader(strings.NewReader(large)), -1, 1<<20)
		if err != nil || len(got.Data) != 5000-len(`{"data":""}`) {
			t.Fatalf("got %d bytes, %v; want the whole body", len(got.Data), err)
		}
	})

	t.Run("byte at a time, over the limit", func(t *testing.T) {
		_, err := bind(iotest.OneByteReader(strings.NewReader(large)), -1, 1024)
		if !errors.Is(err, ErrBodyTooLarge) {
			t.Fatalf("got %v, want ErrBodyTooLarge", err)
		}
	})

	t.Run("byte at a time, no limit", func(t *testing.T) {
		got, err := bind(iotest.OneByteReader(strings.NewReader(large)), -1, -1)
		if err != nil || len(got.Data) != 5000-len(`{"data":""}`) {
			t.Fatalf("got %d bytes, %v; want the whole body", len(got.Data), err)
		}
	})

	t.Run("data with EOF", func(t *testing.T) {
		got, err := bind(iotest.DataErrReader(strings.NewReader(large)), int64(len(large)), 1<<20)
		if err != nil || len(got.Data) != 5000-len(`{"data":""}`) {
			t.Fatalf("got %d bytes, %v; want the whole body", len(got.Data), err)
		}
	})

	t.Run("read error", func(t *testing.T) {
		boom := errors.New("connection reset")
		_, err := bind(io.MultiReader(strings.NewReader(`{"data":`), iotest.ErrReader(boom)), -1, 1024)
		if !errors.Is(err, boom) {
			t.Fatalf("got %v, want the read error", err)
		}
		if errors.Is(err, ErrBodyTooLarge) || errors.Is(err, ErrMalformedBody) {
			t.Errorf("a read failure was misreported as %v", err)
		}
	})
}

// The restored body can be read again in full, and closed.
func TestBodyRestoredAfterBind(t *testing.T) {
	const body = `{"data":"again"}`
	r := httptest.NewRequest("POST", "/u", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	var got sizedRequest
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(r.Body)
	if err != nil || string(rest) != body {
		t.Fatalf("restored body = %q, %v; want %q", rest, err, body)
	}
	if err := r.Body.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// An oversized body used to be left part read, so a later reader saw only
// what binder had not consumed. The body is put back whole.
func TestBodyRestoredAfterTooLarge(t *testing.T) {
	body := jsonBodyOfSize(100)
	r := httptest.NewRequest("POST", "/u", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = -1 // undeclared, so the limit is found by reading

	var got sizedRequest
	if err := bindWithLimit(r, &got, 10); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("got %v, want ErrBodyTooLarge", err)
	}
	rest, err := io.ReadAll(r.Body)
	if err != nil || string(rest) != body {
		t.Fatalf("restored body = %q, %v; want %q", rest, err, body)
	}
	if err := r.Body.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// A target with no body field does not read the body, so it is neither
// limited nor parsed: the handler gets it untouched.
func TestBodyUnreadWithoutBodyFields(t *testing.T) {
	var got struct {
		ID string `header:"X-Id"`
	}
	const body = `{not json`
	r := httptest.NewRequest("POST", "/u", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Id", "7")
	if err := bindWithLimit(r, &got, 1); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
	if got.ID != "7" {
		t.Errorf("ID = %q, want 7", got.ID)
	}
	rest, _ := io.ReadAll(r.Body)
	if string(rest) != body {
		t.Errorf("body = %q, want it unread", rest)
	}

	// Reporting unknown members needs the body, so it is still read then.
	r = httptest.NewRequest("POST", "/u", strings.NewReader(`{"extra":1}`))
	r.Header.Set("Content-Type", "application/json")
	if err := BindWithOptions(r, &got, BindOptions{DisallowUnknownFields: true}); !errors.Is(err, ErrUnknownField) {
		t.Errorf("got %v, want ErrUnknownField", err)
	}
}
