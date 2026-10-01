package binder

import (
	"bytes"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The form tag reads as net/http's r.FormValue does, and as Gin's form tag:
// a form body's value when the body has the key, and otherwise the query's.

func formTagRequest(target, body string) *http.Request {
	r := httptest.NewRequest("POST", target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

type formPage struct {
	Page int      `form:"page"`
	Sort string   `form:"sort"`
	Tags []string `form:"tag"`
}

func TestFormReadsBodyThenQuery(t *testing.T) {
	var got formPage
	r := formTagRequest("/?page=1&sort=name&tag=q1&tag=q2", "page=2&tag=b1&tag=b2")
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	// page and tag are in the body, so the body's values bind; sort is only
	// in the query.
	if got.Page != 2 || got.Sort != "name" || strings.Join(got.Tags, ",") != "b1,b2" {
		t.Errorf("got %+v", got)
	}
}

func TestFormReadsQueryWithoutBody(t *testing.T) {
	var got formPage
	if err := Bind(httptest.NewRequest("GET", "/?page=3&tag=a&tag=b", nil), &got); err != nil {
		t.Fatal(err)
	}
	if got.Page != 3 || strings.Join(got.Tags, ",") != "a,b" {
		t.Errorf("got %+v", got)
	}
}

// A JSON body is not a form, so form fields read the query alone, and a JSON
// member with a form field's name is not bound by it.
func TestFormIgnoresJSONBody(t *testing.T) {
	var got formPage
	r := httptest.NewRequest("POST", "/?page=4", strings.NewReader(`{"page":9,"sort":"x"}`))
	r.Header.Set("Content-Type", "application/json")
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	if got.Page != 4 || got.Sort != "" {
		t.Errorf("got %+v, want page from the query and no sort", got)
	}
}

func TestFormMapBodyThenQuery(t *testing.T) {
	var got struct {
		Filter map[string]string `form:"filter"`
	}
	if err := Bind(formTagRequest("/?filter[a]=q", "filter[b]=body"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Filter) != 1 || got.Filter["b"] != "body" {
		t.Errorf("got %v, want the body's entries", got.Filter)
	}
	got.Filter = nil
	if err := Bind(httptest.NewRequest("GET", "/?filter[a]=q", nil), &got); err != nil {
		t.Fatal(err)
	}
	if got.Filter["a"] != "q" {
		t.Errorf("got %v, want the query's entries", got.Filter)
	}
}

func TestFormRequired(t *testing.T) {
	var got struct {
		Name string `form:"name,required"`
	}
	err := Bind(httptest.NewRequest("GET", "/?name=", nil), &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 1 || !errors.Is(errs[0], ErrMissingRequired) || errs[0].Source != "form" || errs[0].Name != "name" {
		t.Errorf("got %v, want name missing from form", err)
	}
	// A body key that is present but empty satisfies required, as for body.
	if err := Bind(formTagRequest("/", "name="), &got); err != nil {
		t.Errorf("empty body value: %v", err)
	}
}

func TestFormFileUpload(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("title", "t")
	fw, _ := w.CreateFormFile("doc", "d.txt")
	_, _ = fw.Write([]byte("file"))
	_ = w.Close()
	r := httptest.NewRequest("POST", "/", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())

	var got struct {
		Title string                `form:"title"`
		Doc   *multipart.FileHeader `form:"doc"`
	}
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "t" || got.Doc == nil || got.Doc.Filename != "d.txt" {
		t.Errorf("got %+v", got)
	}
}

// Form keys, map entries included, are known to DisallowUnknownFields in a
// form body; in a JSON body they are not, since form does not read one.
func TestFormKeysAreKnown(t *testing.T) {
	var got struct {
		Page   int               `form:"page"`
		Filter map[string]string `form:"filter"`
	}
	opts := BindOptions{DisallowUnknownFields: true}
	err := BindWithOptions(formTagRequest("/", "page=1&filter[a]=b&other=x"), &got, opts)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 1 || errs[0].Name != "other" {
		t.Errorf("form body: got %v, want only other unknown", err)
	}

	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"page":1}`))
	r.Header.Set("Content-Type", "application/json")
	err = BindWithOptions(r, &got, opts)
	if !errors.As(err, &errs) || len(errs) != 1 || errs[0].Name != "page" || !errors.Is(errs[0], ErrUnknownField) {
		t.Errorf("JSON body: got %v, want page unknown", err)
	}
}

// Only form fields read the body: it is read for them alone.
func TestFormOnlyTargetReadsBody(t *testing.T) {
	var got struct {
		Name string `form:"name"`
	}
	if err := Bind(formTagRequest("/", "name=ada"), &got); err != nil || got.Name != "ada" {
		t.Errorf("got %q, %v", got.Name, err)
	}
}

// form comes last in precedence, so a field that also has another tag keeps
// binding from that one.
func TestFormPrecedence(t *testing.T) {
	var got struct {
		A string `query:"a" form:"b"`
	}
	if err := Bind(formTagRequest("/?a=q", "b=f"), &got); err != nil || got.A != "q" {
		t.Errorf("got %q, %v; want the query tag to win", got.A, err)
	}
}

// Form fields are promoted from an embedded struct like any other.
func TestFormEmbedded(t *testing.T) {
	type Paging struct {
		Page int `form:"page"`
	}
	var got struct {
		Paging
		Name string `form:"name"`
	}
	if err := Bind(formTagRequest("/?page=5", "name=n"), &got); err != nil || got.Page != 5 || got.Name != "n" {
		t.Errorf("got %+v, %v", got, err)
	}
}
