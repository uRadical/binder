package binder

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
func bindWithLimit(r *http.Request, target interface{}, limit int64) error {
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

	binds := map[string]func(*http.Request, interface{}) error{
		"Bind": Bind,
		"BindWithOptions zero limit": func(r *http.Request, target interface{}) error {
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
