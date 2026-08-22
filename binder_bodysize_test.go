package binder

import (
	"errors"
	"io"
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

func withMaxBodySize(t *testing.T, limit int64) {
	t.Helper()
	previous := MaxBodySize
	MaxBodySize = limit
	t.Cleanup(func() { MaxBodySize = previous })
}

func TestBodyWithinLimitBinds(t *testing.T) {
	withMaxBodySize(t, 1024)

	body := jsonBodyOfSize(1024)
	r := httptest.NewRequest("POST", "/u", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	var got sizedRequest
	if err := Bind(r, &got); err != nil {
		t.Fatalf("body exactly at the limit: got error %v, want nil", err)
	}
	if len(got.Data) != 1024-len(`{"data":""}`) {
		t.Errorf("Data length = %d, want %d", len(got.Data), 1024-len(`{"data":""}`))
	}
}

func TestBodyOverLimitRejected(t *testing.T) {
	withMaxBodySize(t, 1024)

	r := httptest.NewRequest("POST", "/u", strings.NewReader(jsonBodyOfSize(1025)))
	r.Header.Set("Content-Type", "application/json")

	var got sizedRequest
	err := Bind(r, &got)
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
	withMaxBodySize(t, 1024)

	r := httptest.NewRequest("POST", "/u", strings.NewReader(jsonBodyOfSize(64<<10)))
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = 32 // a lie

	var got sizedRequest
	err := Bind(r, &got)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("understated Content-Length: got %v, want ErrBodyTooLarge", err)
	}
}

// An honestly declared oversized body is refused without being read.
func TestOversizedContentLengthNotRead(t *testing.T) {
	withMaxBodySize(t, 1024)

	tripwire := &trackingReader{Reader: strings.NewReader(jsonBodyOfSize(4096))}
	r := httptest.NewRequest("POST", "/u", tripwire)
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = 4096

	var got sizedRequest
	if err := Bind(r, &got); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("declared oversized body: got %v, want ErrBodyTooLarge", err)
	}
	if tripwire.reads != 0 {
		t.Errorf("body was read %d times, want 0 - an oversized body should be refused unread", tripwire.reads)
	}
}

// A limit of zero or less restores unbounded reading.
func TestZeroLimitDisablesCap(t *testing.T) {
	withMaxBodySize(t, 0)

	r := httptest.NewRequest("POST", "/u", strings.NewReader(jsonBodyOfSize(256<<10)))
	r.Header.Set("Content-Type", "application/json")

	var got sizedRequest
	if err := Bind(r, &got); err != nil {
		t.Fatalf("limit disabled: got error %v, want nil", err)
	}
	if len(got.Data) == 0 {
		t.Error("body was not bound with the limit disabled")
	}
}

func TestDefaultMaxBodySize(t *testing.T) {
	if MaxBodySize != DefaultMaxBodySize {
		t.Errorf("MaxBodySize = %d, want DefaultMaxBodySize (%d)", MaxBodySize, DefaultMaxBodySize)
	}
	if DefaultMaxBodySize != 10<<20 {
		t.Errorf("DefaultMaxBodySize = %d, want 10 MB", DefaultMaxBodySize)
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
