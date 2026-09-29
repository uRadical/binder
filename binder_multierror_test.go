package binder

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

type multiTarget struct {
	Page  int    `query:"page"`
	Limit int    `query:"limit"`
	Name  string `body:"name,required"`
	Age   int    `body:"age"`
	Score int    `body:"score"`
	ran   *bool
}

func (m multiTarget) Validate(context.Context) error {
	if m.ran != nil {
		*m.ran = true
	}
	return nil
}

// bindErrors extracts the BindErrors from err, failing the test without them.
func bindErrors(t *testing.T, err error) BindErrors {
	t.Helper()
	var errs BindErrors
	if !errors.As(err, &errs) {
		t.Fatalf("got %v (%T), want BindErrors", err, err)
	}
	return errs
}

// paths renders each failure as Field=Name.
func paths(errs BindErrors) string {
	var out []string
	for _, e := range errs {
		out = append(out, e.Field+"="+e.Name)
	}
	return strings.Join(out, ",")
}

// Every failing field is reported, in field order, whichever source and body
// format it came from.
func TestBindReportsEveryFieldError(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
	}{
		{"json", "application/json", `{"score":"x","age":"y"}`},
		{"form", "application/x-www-form-urlencoded", `score=x&age=y`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/?page=a&limit=b", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)

			ran := false
			v := multiTarget{ran: &ran}
			err := Bind(r, &v)

			want := "Page=page,Limit=limit,Name=name,Age=age,Score=score"
			if got := paths(bindErrors(t, err)); got != want {
				t.Fatalf("got %s, want %s\nerror: %v", got, want, err)
			}
			if !errors.Is(err, ErrMissingRequired) {
				t.Errorf("errors.Is(err, ErrMissingRequired) = false for %v", err)
			}
			if ran {
				t.Error("Validate ran although binding failed")
			}
		})
	}
}

// One failure has the same shape as several.
func TestBindSingleFailureIsBindErrors(t *testing.T) {
	r := httptest.NewRequest("GET", "/?page=a", nil)
	var v struct {
		Page int `query:"page"`
	}
	err := Bind(r, &v)
	if errs := bindErrors(t, err); len(errs) != 1 || errs[0].Field != "Page" {
		t.Fatalf("got %v, want one failure for Page", errs)
	}
	var bindErr *BindError
	if !errors.As(err, &bindErr) || bindErr.Name != "page" {
		t.Fatalf("errors.As should reach the *BindError, got %v", bindErr)
	}
}

// Each unknown member is its own failure, after the fields that failed.
func TestBindUnknownFieldsAreBindErrors(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"zed":1,"age":"y","extra":1}`))
	r.Header.Set("Content-Type", "application/json")
	var v struct {
		Age int `body:"age"`
	}
	err := BindWithOptions(r, &v, BindOptions{DisallowUnknownFields: true})
	errs := bindErrors(t, err)
	if got, want := paths(errs), "Age=age,=extra,=zed"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if !errors.Is(errs[1], ErrUnknownField) || !errors.Is(err, ErrUnknownField) {
		t.Fatalf("unknown members should match ErrUnknownField: %v", err)
	}
}

// A body that cannot be read still ends binding at once, and is not a
// BindErrors.
func TestBindMalformedBodyStillStops(t *testing.T) {
	r := httptest.NewRequest("POST", "/?page=a", strings.NewReader(`{"age":`))
	r.Header.Set("Content-Type", "application/json")
	var v multiTarget
	err := Bind(r, &v)
	var errs BindErrors
	if !errors.Is(err, ErrMalformedBody) || errors.As(err, &errs) {
		t.Fatalf("got %v, want a lone ErrMalformedBody", err)
	}
}

// Failures within nested structs and slices are entries of their own, named
// by their path.
func TestBindNestedFailuresArePaths(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(
		`{"inner":{"a":"x","b":"y","deep":{"c":"z"}},"items":[{"n":1},{"n":"q"}],"tags":[1,"w",3]}`))
	r.Header.Set("Content-Type", "application/json")
	type item struct {
		N int `body:"n"`
	}
	var v struct {
		Inner struct {
			A    int `body:"a"`
			B    int `body:"b"`
			Deep struct {
				C int `json:"c"`
			} `body:"deep"`
		} `body:"inner"`
		Items []item `body:"items"`
		Tags  []int  `body:"tags"`
	}
	errs := bindErrors(t, Bind(r, &v))
	want := "Inner.A=inner.a,Inner.B=inner.b,Inner.Deep.C=inner.deep.c,Items[1].N=items[1].n,Tags[1]=tags[1]"
	if got := paths(errs); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	for _, e := range errs {
		if e.Source != body || !strings.HasPrefix(e.Message, "error setting field "+e.Field+": ") {
			t.Errorf("entry %+v: want source body and a message naming its path", e)
		}
	}
}

type joinedValidation struct {
	Name  string `query:"name"`
	Email string `query:"email"`
}

var errNoName, errNoEmail = errors.New("name is required"), errors.New("email is required")

func (j joinedValidation) Validate(context.Context) error {
	var errs []error
	if j.Name == "" {
		errs = append(errs, errNoName)
	}
	if j.Email == "" {
		errs = append(errs, errNoEmail)
	}
	return errors.Join(errs...)
}

// What Validate returns is the caller's, and reaches them unchanged.
func TestBindValidationErrorsAreTheCallers(t *testing.T) {
	err := Bind(httptest.NewRequest("GET", "/", nil), &joinedValidation{})
	if !strings.HasPrefix(err.Error(), "validation failed: ") {
		t.Fatalf("got %q, want the validation prefix", err)
	}
	var errs BindErrors
	if errors.As(err, &errs) {
		t.Fatal("a validation failure should not be BindErrors")
	}
	if !errors.Is(err, errNoName) || !errors.Is(err, errNoEmail) {
		t.Fatalf("errors.Is should reach each joined validation error: %v", err)
	}
}
