package binder

import (
	"bytes"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Repeated form fields used to fail the whole bind with
// "cannot convert []string to slice", even though parseBody produced the
// []string deliberately.
func TestRepeatedFormFieldsBindToSlice(t *testing.T) {
	var got struct {
		Tags []string `body:"tags"`
	}
	r := httptest.NewRequest("POST", "/s", strings.NewReader("tags=a&tags=b&tags=c"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Tags) != 3 || got.Tags[0] != "a" || got.Tags[2] != "c" {
		t.Errorf("Tags = %v, want [a b c]", got.Tags)
	}
}

func TestRepeatedQueryParamsBindToSlice(t *testing.T) {
	var got struct {
		Tags []string `query:"tags"`
	}
	r := httptest.NewRequest("GET", "/s?tags=a&tags=b&tags=c", nil)

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Tags) != 3 || got.Tags[0] != "a" || got.Tags[2] != "c" {
		t.Errorf("Tags = %v, want [a b c]", got.Tags)
	}
}

func TestRepeatedHeadersBindToSlice(t *testing.T) {
	var got struct {
		Accept []string `header:"X-Accept"`
	}
	r := httptest.NewRequest("GET", "/s", nil)
	r.Header.Add("X-Accept", "a")
	r.Header.Add("X-Accept", "b")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Accept) != 2 || got.Accept[0] != "a" || got.Accept[1] != "b" {
		t.Errorf("Accept = %v, want [a b]", got.Accept)
	}
}

// A single value into a slice still yields a one-element slice.
func TestSingleValueIntoSliceUnchanged(t *testing.T) {
	var q struct {
		Tags []string `query:"tags"`
	}
	if err := Bind(httptest.NewRequest("GET", "/s?tags=a", nil), &q); err != nil {
		t.Fatalf("query: got error %v, want nil", err)
	}
	if len(q.Tags) != 1 || q.Tags[0] != "a" {
		t.Errorf("Tags = %v, want [a]", q.Tags)
	}

	var f struct {
		Tags []string `body:"tags"`
	}
	r := httptest.NewRequest("POST", "/s", strings.NewReader("tags=a"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := Bind(r, &f); err != nil {
		t.Fatalf("form: got error %v, want nil", err)
	}
	if len(f.Tags) != 1 || f.Tags[0] != "a" {
		t.Errorf("Tags = %v, want [a]", f.Tags)
	}
}

// A non-slice field keeps taking the first value.
func TestNonSliceFieldTakesFirstValue(t *testing.T) {
	var got struct {
		Tag string `query:"tags"`
	}
	if err := Bind(httptest.NewRequest("GET", "/s?tags=a&tags=b", nil), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Tag != "a" {
		t.Errorf("Tag = %q, want %q", got.Tag, "a")
	}
}

// Elements convert like any other value.
func TestRepeatedValuesConvertElementTypes(t *testing.T) {
	var got struct {
		IDs   []int  `query:"id"`
		Flags []bool `query:"flag"`
	}
	r := httptest.NewRequest("GET", "/s?id=1&id=2&id=3&flag=true&flag=false", nil)

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.IDs) != 3 || got.IDs[0] != 1 || got.IDs[2] != 3 {
		t.Errorf("IDs = %v, want [1 2 3]", got.IDs)
	}
	if len(got.Flags) != 2 || !got.Flags[0] || got.Flags[1] {
		t.Errorf("Flags = %v, want [true false]", got.Flags)
	}
}

// A bad element reports the field, not a bare conversion failure.
func TestBadRepeatedElementReportsField(t *testing.T) {
	var got struct {
		IDs []int `query:"id"`
	}
	err := Bind(httptest.NewRequest("GET", "/s?id=1&id=nope", nil), &got)
	if err == nil {
		t.Fatal("got nil error")
	}

	var bindErr *BindError
	if !errors.As(err, &bindErr) {
		t.Fatalf("errors.As(*BindError) = false for %v", err)
	}
	if bindErr.Field != "IDs[1]" || bindErr.Name != "id[1]" {
		t.Errorf("Field, Name = %q, %q, want %q, %q", bindErr.Field, bindErr.Name, "IDs[1]", "id[1]")
	}
}

// A slice field with no values is absent, so required fires and omitempty
// has nothing to skip.
func TestRequiredSliceWithNoValues(t *testing.T) {
	var got struct {
		Tags []string `query:"tags,required"`
	}
	err := Bind(httptest.NewRequest("GET", "/s", nil), &got)
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("got %v, want ErrMissingRequired", err)
	}
}

func TestAbsentSliceLeavesFieldAlone(t *testing.T) {
	var got struct {
		Tags []string `query:"tags"`
	}
	got.Tags = []string{"preexisting"}

	if err := Bind(httptest.NewRequest("GET", "/s", nil), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "preexisting" {
		t.Errorf("Tags = %v, want it untouched", got.Tags)
	}
}

// JSON arrays are unaffected.
func TestJSONArraysUnaffected(t *testing.T) {
	var got struct {
		Tags []string `body:"tags"`
	}
	r := httptest.NewRequest("POST", "/s", strings.NewReader(`{"tags":["a","b"]}`))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "a" || got.Tags[1] != "b" {
		t.Errorf("Tags = %v, want [a b]", got.Tags)
	}
}

// A comma in a single value stays one value: splitting is a convention this
// package does not impose.
func TestCommaInValueIsNotSplit(t *testing.T) {
	var got struct {
		Tags []string `query:"tags"`
	}
	if err := Bind(httptest.NewRequest("GET", "/s?tags=a,b", nil), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "a,b" {
		t.Errorf("Tags = %v, want [\"a,b\"]", got.Tags)
	}
}

// A required header slice with no values must fail, the same as a required
// query slice. Only the query case was covered, so the header branch could
// have reported an empty slice as present and nothing would have noticed.
func TestRequiredHeaderSliceWithNoValues(t *testing.T) {
	var got struct {
		Accept []string `header:"X-Accept,required"`
	}
	err := Bind(httptest.NewRequest("GET", "/s", nil), &got)
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("got %v, want ErrMissingRequired", err)
	}
}

// An absent header slice leaves the field alone.
func TestAbsentHeaderSliceLeavesFieldAlone(t *testing.T) {
	var got struct {
		Accept []string `header:"X-Accept"`
	}
	got.Accept = []string{"preexisting"}

	if err := Bind(httptest.NewRequest("GET", "/s", nil), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Accept) != 1 || got.Accept[0] != "preexisting" {
		t.Errorf("Accept = %v, want it untouched", got.Accept)
	}
}

// A form field sent twice used to bind into a string as "[x y]", the
// []string formatted with %v, and into an int as a conversion error. A field
// that takes one value binds the first, as a repeated query parameter does.
func TestRepeatedFormFieldTakesFirstValue(t *testing.T) {
	for _, ct := range []string{"application/x-www-form-urlencoded", "multipart/form-data"} {
		t.Run(ct, func(t *testing.T) {
			var got struct {
				S string `body:"s"`
				N int    `body:"n"`
				P *int   `body:"p"`
			}
			if err := Bind(formRequest(t, ct, "s", "x", "s", "y", "n", "1", "n", "2", "p", "3", "p", "4"), &got); err != nil {
				t.Fatalf("got error %v, want nil", err)
			}
			if got.S != "x" || got.N != 1 || got.P == nil || *got.P != 3 {
				t.Errorf("got S=%q N=%d P=%v, want x, 1, 3", got.S, got.N, got.P)
			}
		})
	}
}

// A single form value used to bind only into []string; any other element type
// failed with "cannot convert string to slice".
func TestSingleFormValueIntoTypedSlice(t *testing.T) {
	for _, ct := range []string{"application/x-www-form-urlencoded", "multipart/form-data"} {
		t.Run(ct, func(t *testing.T) {
			var got struct {
				Scores []int     `body:"scores"`
				Ptrs   []*string `body:"ptrs"`
			}
			if err := Bind(formRequest(t, ct, "scores", "5", "ptrs", "a"), &got); err != nil {
				t.Fatalf("got error %v, want nil", err)
			}
			if len(got.Scores) != 1 || got.Scores[0] != 5 {
				t.Errorf("Scores = %v, want [5]", got.Scores)
			}
			if len(got.Ptrs) != 1 || *got.Ptrs[0] != "a" {
				t.Errorf("Ptrs = %v, want [a]", got.Ptrs)
			}
		})
	}
}

// A JSON scalar where a list is expected binds as a one-element list, as a
// single form value does. An object does not.
func TestSingleJSONValueIntoSlice(t *testing.T) {
	var got struct {
		Scores []int    `body:"scores"`
		Tags   []string `body:"tags"`
	}
	r := httptest.NewRequest("POST", "/s", strings.NewReader(`{"scores":5,"tags":"a"}`))
	r.Header.Set("Content-Type", "application/json")
	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Scores) != 1 || got.Scores[0] != 5 || len(got.Tags) != 1 || got.Tags[0] != "a" {
		t.Errorf("got Scores=%v Tags=%v, want [5] [a]", got.Scores, got.Tags)
	}

	r = httptest.NewRequest("POST", "/s", strings.NewReader(`{"scores":{"a":1}}`))
	r.Header.Set("Content-Type", "application/json")
	if err := Bind(r, &got); err == nil {
		t.Error("object into []int bound, want an error")
	}
}

// An empty value is absent for a slice field as it is for a field taking one
// value: ?q= used to bind [""] and satisfy required.
func TestEmptyValuesIntoSliceAreAbsent(t *testing.T) {
	var got struct {
		Q []string `query:"q,required"`
		H []string `header:"X-H,required"`
	}
	r := httptest.NewRequest("GET", "/s?q=&q=", nil)
	r.Header["X-H"] = []string{""}
	err := Bind(r, &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 2 || !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("got %v, want two ErrMissingRequired entries", err)
	}
	if got.Q != nil || got.H != nil {
		t.Errorf("got Q=%q H=%q, want both left nil", got.Q, got.H)
	}

	// A non-empty value among empty ones is present, and every value binds.
	var opt struct {
		Q []string `query:"q"`
	}
	if err := Bind(httptest.NewRequest("GET", "/s?q=&q=a", nil), &opt); err != nil {
		t.Fatal(err)
	}
	if len(opt.Q) != 2 || opt.Q[1] != "a" {
		t.Errorf("Q = %q, want [\"\" a]", opt.Q)
	}
}

// formRequest builds a POST whose body carries the given name, value pairs in
// either form encoding.
func formRequest(t *testing.T, contentType string, pairs ...string) *http.Request {
	t.Helper()
	if contentType != "multipart/form-data" {
		v := url.Values{}
		for i := 0; i < len(pairs); i += 2 {
			v.Add(pairs[i], pairs[i+1])
		}
		r := httptest.NewRequest("POST", "/s", strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", contentType)
		return r
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for i := 0; i < len(pairs); i += 2 {
		if err := w.WriteField(pairs[i], pairs[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/s", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

// Form bodies used to go through Request.ParseForm, which ignored the body on
// GET and DELETE, capped it at 10 MB whatever MaxBodySize said, and failed on
// a malformed URL query as if the body were at fault.
func TestFormBodyParsedLikeAnyBody(t *testing.T) {
	type target struct {
		A string `body:"a"`
		Q string `query:"q"`
	}
	form := func(method, target, body string) *http.Request {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}

	var got target
	if err := Bind(form("DELETE", "/s", "a=hello"), &got); err != nil || got.A != "hello" {
		t.Errorf("DELETE: got %v, A=%q; want nil, hello", err, got.A)
	}

	got = target{}
	if err := Bind(form("POST", "/s?q=%zz&x=1;y=2", "a=hello"), &got); err != nil || got.A != "hello" {
		t.Errorf("bad query: got %v, A=%q; want nil, hello", err, got.A)
	}

	got = target{}
	big := "a=" + strings.Repeat("x", 11<<20)
	if err := BindWithOptions(form("POST", "/s", big), &got, BindOptions{MaxBodySize: -1}); err != nil || len(got.A) != 11<<20 {
		t.Errorf("11 MB unlimited: got %v, len(A)=%d", err, len(got.A))
	}

	// A malformed body is still malformed.
	if err := Bind(form("POST", "/s", "a=%zz"), &got); !errors.Is(err, ErrMalformedBody) {
		t.Errorf("bad body: got %v, want ErrMalformedBody", err)
	}
}
