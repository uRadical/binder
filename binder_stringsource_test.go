package binder

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// Path, query, header and cookie values are strings and are set without
// passing through an interface. This pins that path for every kind of
// destination, and the general path it falls back to for the rest.
func TestStringSourceKinds(t *testing.T) {
	type target struct {
		S   string        `query:"s"`
		I   int           `query:"i"`
		I8  int8          `query:"i8"`
		U   uint          `query:"u"`
		U8  uint8         `query:"u8"`
		F   float64       `query:"f"`
		F32 float32       `query:"f32"`
		B   bool          `query:"b"`
		P   *int          `query:"p"` // pointer: general path
		D   time.Duration `query:"d"` // named type: general path
		T   time.Time     `query:"t"` // TextUnmarshaler: general path
		H   uint16        `header:"X-Count"`
		C   float32       `cookie:"ratio"`
	}

	r := httptest.NewRequest("GET", "/?s=hi&i=-7&i8=-8&u=7&u8=255&f=1.5&f32=2.5&b=true&p=3&d=9&t=2026-01-02T03:04:05Z", nil)
	r.Header.Set("X-Count", "65535")
	r.AddCookie(&http.Cookie{Name: "ratio", Value: "0.25"})

	var got target
	if err := Bind(r, &got); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	want := target{S: "hi", I: -7, I8: -8, U: 7, U8: 255, F: 1.5, F32: 2.5, B: true, D: 9,
		T: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), H: 65535, C: 0.25}
	if got.P == nil || *got.P != 3 {
		t.Errorf("P = %v, want pointer to 3", got.P)
	}
	got.P = nil
	if got != want {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// A string that does not convert is reported against its field, with the
// parse error underneath, for each kind the direct path parses.
func TestStringSourceConversionErrors(t *testing.T) {
	var got struct {
		I  int     `query:"i"`
		U  uint    `query:"u"`
		U8 uint8   `query:"u8"`
		I8 int8    `query:"i8"`
		F  float64 `query:"f"`
		B  bool    `query:"b"`
	}
	r := httptest.NewRequest("GET", "/?i=x&u=-1&u8=256&i8=128&f=y&b=maybe", nil)

	var errs BindErrors
	if !errors.As(Bind(r, &got), &errs) {
		t.Fatal("want BindErrors")
	}
	if len(errs) != 6 {
		t.Fatalf("got %d failures, want 6: %v", len(errs), errs)
	}
	var numErr *strconv.NumError
	for _, i := range []int{0, 1, 4, 5} { // parse failures, not overflows
		if !errors.As(errs[i], &numErr) {
			t.Errorf("%s: %v does not wrap a *strconv.NumError", errs[i].Name, errs[i])
		}
	}
	for _, i := range []int{2, 3} { // parsed, but the field cannot hold it
		if errs[i].Field != [...]string{2: "U8", 3: "I8"}[i] {
			t.Errorf("entry %d is %s, want the overflowing field", i, errs[i].Field)
		}
	}
}

// A cookie can be present but empty, unlike path, query and header values.
// omitempty skips it, and without omitempty it is converted like any value.
func TestEmptyCookie(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", "n=")

	var skipped struct {
		N int `cookie:"n,omitempty"`
	}
	if err := Bind(r, &skipped); err != nil {
		t.Errorf("omitempty: got %v, want the empty cookie skipped", err)
	}

	var converted struct {
		N int `cookie:"n"`
	}
	if err := Bind(r, &converted); err == nil {
		t.Error("without omitempty: got nil, want an empty string to fail to convert")
	}

	var required struct {
		S string `cookie:"n,required"`
	}
	if err := Bind(r, &required); err != nil {
		t.Errorf("required: got %v, want a present empty cookie to satisfy it", err)
	}
}
