package binder

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"
)

// The fuzz targets below drive Bind with arbitrary request data. Binding is a
// reflection walk over untrusted input, so the contract under fuzzing is that
// it always returns, never panics, and leaves the body readable.

type fuzzScalars struct {
	I8   int8    `body:"i8"`
	I64  int64   `body:"i64"`
	U8   uint8   `body:"u8"`
	U64  uint64  `body:"u64"`
	F32  float32 `body:"f32"`
	F64  float64 `body:"f64"`
	B    bool    `body:"b"`
	S    string  `body:"s"`
	Ptr  *int    `body:"ptr"`
	PtrS *string `body:"ptrs"`
}

type fuzzNested struct {
	Inner struct {
		Deep struct {
			When *time.Time `body:"when"`
			ID   *uuid.UUID `body:"id"`
			N    int        `body:"n"`
		} `body:"deep"`
		Tags []string `body:"tags"`
	} `body:"inner"`
}

type fuzzSlices struct {
	Strs  []string    `body:"strs"`
	Ints  []int       `body:"ints"`
	Ptrs  []*int      `body:"ptrs"`
	Query []string    `query:"q"`
	Head  []string    `header:"X-Multi"`
	Times []time.Time `body:"times"`
}

type fuzzSources struct {
	Path   int       `path:"id"`
	Query  string    `query:"q,omitempty"`
	Cookie string    `cookie:"c"`
	Header string    `header:"X-H"`
	Body   string    `body:"s,required"`
	When   time.Time `header:"X-When"`
}

// fuzzTarget returns a fresh binding destination chosen by the fuzzer.
func fuzzTarget(which uint8) interface{} {
	switch which % 4 {
	case 0:
		return &fuzzScalars{}
	case 1:
		return &fuzzNested{}
	case 2:
		return &fuzzSlices{}
	default:
		return &fuzzSources{}
	}
}

func FuzzBind(f *testing.F) {
	f.Add(uint8(0), "application/json", `{"i8":1,"i64":9007199254740993,"s":"x"}`, "q=1", "42", "c=v", "h")
	f.Add(uint8(1), "application/json", `{"inner":{"deep":{"when":"2026-01-02T03:04:05Z","id":"f47ac10b-58cc-0372-8562-0b8e853961a1"}}}`, "", "1", "", "")
	f.Add(uint8(2), "application/json", `{"strs":["a","b"],"ints":[1,2]}`, "q=a&q=b", "1", "", "")
	f.Add(uint8(3), "application/x-www-form-urlencoded", "s=v&s=w", "q=x", "7", "c=k", "hv")
	f.Add(uint8(0), "application/vnd.api+json", `{"f64":1e309}`, "", "", "", "")
	f.Add(uint8(0), "text/plain", "not json at all", "", "", "", "")
	f.Add(uint8(1), "application/json", `{`, "", "", "", "")
	f.Add(uint8(2), "application/json", `{"strs":null,"ints":[null]}`, "", "", "", "")

	f.Fuzz(func(t *testing.T, which uint8, contentType, body, query, pathValue, cookie, header string) {
		// Header and cookie values containing control characters cannot be
		// carried by a real request, so skip what net/http would reject.
		if !isHeaderSafe(contentType) || !isHeaderSafe(header) || !isHeaderSafe(cookie) {
			t.Skip()
		}

		r := httptest.NewRequest("POST", "/u", strings.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		if header != "" {
			r.Header.Set("X-H", header)
			r.Header.Set("X-Multi", header)
			r.Header.Set("X-When", header)
		}
		if cookie != "" {
			r.Header.Set("Cookie", cookie)
		}
		if query != "" {
			r.URL.RawQuery = query
		}
		r.SetPathValue("id", pathValue)

		target := fuzzTarget(which)

		// The contract: Bind returns, whatever it was given.
		_ = Bind(r, target)

		// And it leaves the body readable for whatever runs next.
		if r.Body == nil {
			t.Fatal("Bind left a nil body behind")
		}
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Fatalf("body unreadable after Bind: %v", err)
		}
	})
}

// FuzzBindOptions drives the same input through the option-carrying entry
// point, so unknown-field detection and per-call limits are covered too.
func FuzzBindWithOptions(f *testing.F) {
	f.Add(`{"s":"x","unknown":1}`, int64(1024), true)
	f.Add(`{"s":""}`, int64(-1), false)
	f.Add(`{}`, int64(1), true)

	f.Fuzz(func(t *testing.T, body string, limit int64, disallow bool) {
		r := httptest.NewRequest("POST", "/u", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")

		var got fuzzScalars
		_ = BindWithOptions(r, &got, BindOptions{
			MaxBodySize:           limit,
			DisallowUnknownFields: disallow,
		})
	})
}

// FuzzBindStruct covers the exported nested-binding entry point directly.
func FuzzBindStruct(f *testing.F) {
	f.Add("k", "v")
	f.Add("when", "2026-01-02T03:04:05Z")

	f.Fuzz(func(t *testing.T, key, value string) {
		var target fuzzNested
		_ = BindStruct(reflect.ValueOf(&target).Elem(), map[string]interface{}{key: value})
	})
}

func isHeaderSafe(s string) bool {
	if len(s) > 4096 {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f || c > 0xff {
			return false
		}
	}
	return true
}

var _ = http.MethodPost
