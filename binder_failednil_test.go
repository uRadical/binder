package binder

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ptrCents decodes itself from a JSON number and refuses anything else.
type ptrCents int64

func (c *ptrCents) UnmarshalJSON(b []byte) error {
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*c = ptrCents(f * 100)
	return nil
}

type ptrInner struct {
	A int `body:"a"`
	B int `body:"b"`
}

// A pointer field whose value fails to bind is left nil, not pointing at a
// zero value, so a nil pointer always means nothing was bound through it.
func TestFailedPointerStaysNil(t *testing.T) {
	type target struct {
		Q     *int           `query:"q"`
		D     *time.Duration `query:"d"`
		H     *int           `header:"X-N"`
		C     *int           `cookie:"c"`
		B     *int           `body:"b"`
		PP    **int          `body:"pp"`
		S     *ptrInner      `body:"s"`
		L     *[]int         `body:"l"`
		M     *ptrCents      `body:"m"`
		Bytes *[]byte        `body:"bytes"`
		O     *int           `body:"o,omitempty"`
		OS    *ptrInner      `body:"os,omitempty"`
		N     struct {
			P  *int      `body:"p"`
			PO *int      `body:"po"`
			SO *ptrInner `body:"so"`
			MO *ptrCents `body:"mo"`
		} `body:"n"`
	}
	r := httptest.NewRequest("POST", "/?q=x&d=soon", strings.NewReader(
		`{"b":"x","pp":"x","s":{"a":1,"b":"x"},"l":[1,"x"],"m":"x","bytes":"!!","o":"x","os":{"b":"x"},"n":{"p":"x","po":"x","so":{"a":1,"b":"x"},"mo":"x"}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-N", "x")
	r.AddCookie(&http.Cookie{Name: "c", Value: "x"})
	var got target
	var errs BindErrors
	if err := Bind(r, &got); !errors.As(err, &errs) || len(errs) != 16 {
		t.Fatalf("got %v, want 16 failures", err)
	}
	v := reflect.ValueOf(got)
	for i := range v.NumField() {
		if f := v.Field(i); f.Kind() == reflect.Pointer && !f.IsNil() {
			t.Errorf("%s = %v, want nil", v.Type().Field(i).Name, f.Elem())
		}
	}
	if got.N.P != nil || got.N.PO != nil || got.N.SO != nil || got.N.MO != nil {
		t.Errorf("N = %+v, want its pointers nil", got.N)
	}

	var form struct {
		B *int `body:"b"`
	}
	r = httptest.NewRequest("POST", "/", strings.NewReader("b=x"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := Bind(r, &form); err == nil || form.B != nil {
		t.Errorf("form: got %v, B = %v; want a failure and nil", err, form.B)
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("doc", "text")
	fw, _ := w.CreateFormFile("doc", "d.txt")
	_, _ = fw.Write([]byte("file"))
	_ = w.Close()
	r = httptest.NewRequest("POST", "/", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	var upload struct {
		Doc *multipart.FileHeader `body:"doc"`
	}
	if err := Bind(r, &upload); err == nil || upload.Doc != nil {
		t.Errorf("upload: got %v, Doc = %v; want a failure and nil", err, upload.Doc)
	}
}

// A pointer the caller set beforehand is not taken away by a failure, and one
// whose value binds is allocated as before.
func TestFailureKeepsCallersPointer(t *testing.T) {
	seven := 7
	got := struct {
		A *int `query:"a"`
		B *int `query:"b"`
	}{A: &seven}
	if err := Bind(httptest.NewRequest("GET", "/?a=x&b=2", nil), &got); err == nil {
		t.Fatal("got nil, want the failure for a")
	}
	if got.A != &seven || got.B == nil || *got.B != 2 {
		t.Errorf("A = %v, B = %v; want the caller's pointer and 2", got.A, got.B)
	}
}
