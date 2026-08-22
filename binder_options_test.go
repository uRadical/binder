package binder

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The zero BindOptions must behave exactly as Bind does.
func TestBindWithOptionsZeroValueMatchesBind(t *testing.T) {
	type request struct {
		Email string `body:"email"`
	}

	newReq := func() *http.Request {
		r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email":"a@b.c"}`))
		r.Header.Set("Content-Type", "application/json")
		return r
	}

	var viaBind, viaOptions request
	if err := Bind(newReq(), &viaBind); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := BindWithOptions(newReq(), &viaOptions, BindOptions{}); err != nil {
		t.Fatalf("BindWithOptions: %v", err)
	}
	if viaBind != viaOptions {
		t.Errorf("BindWithOptions with zero options = %+v, want %+v", viaOptions, viaBind)
	}
}

func TestOptionsMaxBodySizeOverridesPackage(t *testing.T) {
	withMaxBodySize(t, 64<<10) // generous package-level setting

	r := httptest.NewRequest("POST", "/u", strings.NewReader(jsonBodyOfSize(4096)))
	r.Header.Set("Content-Type", "application/json")

	var got sizedRequest
	err := BindWithOptions(r, &got, BindOptions{MaxBodySize: 1024})
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("per-call limit of 1024 against a 4096 byte body: got %v, want ErrBodyTooLarge", err)
	}
}

// A negative per-call limit lifts a package-level limit for one call.
func TestOptionsNegativeMaxBodySizeDisablesLimit(t *testing.T) {
	withMaxBodySize(t, 1024)

	r := httptest.NewRequest("POST", "/u", strings.NewReader(jsonBodyOfSize(8192)))
	r.Header.Set("Content-Type", "application/json")

	var got sizedRequest
	if err := BindWithOptions(r, &got, BindOptions{MaxBodySize: -1}); err != nil {
		t.Fatalf("negative per-call limit: got error %v, want nil", err)
	}
	if got.Data == "" {
		t.Error("body was not bound with the per-call limit disabled")
	}
}

// Zero leaves the package setting in force.
func TestOptionsZeroMaxBodySizeInheritsPackage(t *testing.T) {
	withMaxBodySize(t, 1024)

	r := httptest.NewRequest("POST", "/u", strings.NewReader(jsonBodyOfSize(4096)))
	r.Header.Set("Content-Type", "application/json")

	var got sizedRequest
	if err := BindWithOptions(r, &got, BindOptions{}); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("zero per-call limit: got %v, want the package limit to apply", err)
	}
}

func TestDisallowUnknownFields(t *testing.T) {
	type request struct {
		Email string `body:"email"`
		Nick  string `json:"nick"`
	}

	t.Run("rejects unknown keys", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email":"a@b.c","surprise":1}`))
		r.Header.Set("Content-Type", "application/json")

		var got request
		err := BindWithOptions(r, &got, BindOptions{DisallowUnknownFields: true})
		if !errors.Is(err, ErrUnknownField) {
			t.Fatalf("got %v, want ErrUnknownField", err)
		}
		if !strings.Contains(err.Error(), `"surprise"`) {
			t.Errorf("error %q does not name the unknown key", err)
		}
	})

	t.Run("accepts known keys from both tags", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email":"a@b.c","nick":"al"}`))
		r.Header.Set("Content-Type", "application/json")

		var got request
		if err := BindWithOptions(r, &got, BindOptions{DisallowUnknownFields: true}); err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
	})

	t.Run("off by default", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email":"a@b.c","surprise":1}`))
		r.Header.Set("Content-Type", "application/json")

		var got request
		if err := Bind(r, &got); err != nil {
			t.Fatalf("Bind must ignore unknown keys: got %v", err)
		}
	})

	t.Run("reports every unknown key in a stable order", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"zeta":1,"alpha":2}`))
		r.Header.Set("Content-Type", "application/json")

		var got request
		err := BindWithOptions(r, &got, BindOptions{DisallowUnknownFields: true})
		if err == nil {
			t.Fatal("got nil error")
		}
		if !strings.Contains(err.Error(), `"alpha", "zeta"`) {
			t.Errorf("error %q does not list both keys in sorted order", err)
		}
	})

	t.Run("tag options do not hide a known key", func(t *testing.T) {
		var got struct {
			Email string `body:"email,omitempty"`
		}
		r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email":"a@b.c"}`))
		r.Header.Set("Content-Type", "application/json")

		if err := BindWithOptions(r, &got, BindOptions{DisallowUnknownFields: true}); err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
	})
}

// BindError must expose the failing field and its source.
func TestBindErrorCarriesFieldDetail(t *testing.T) {
	var got struct {
		ID int `query:"id"`
	}
	r := httptest.NewRequest("GET", "/u?id=notanumber", nil)

	err := Bind(r, &got)
	if err == nil {
		t.Fatal("got nil error")
	}

	var bindErr *BindError
	if !errors.As(err, &bindErr) {
		t.Fatalf("errors.As(*BindError) = false for %T: %v", err, err)
	}
	if bindErr.Field != "ID" {
		t.Errorf("Field = %q, want %q", bindErr.Field, "ID")
	}
	if bindErr.Source != query {
		t.Errorf("Source = %q, want %q", bindErr.Source, query)
	}
	if bindErr.Name != "id" {
		t.Errorf("Name = %q, want %q", bindErr.Name, "id")
	}
	if bindErr.Err == nil {
		t.Error("Err is nil, want the underlying conversion failure")
	}
}

// A missing required field is a BindError wrapping ErrMissingRequired.
func TestBindErrorForMissingRequired(t *testing.T) {
	var got struct {
		Email string `body:"email,required"`
	}
	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")

	err := Bind(r, &got)
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("errors.Is(err, ErrMissingRequired) = false for %v", err)
	}

	var bindErr *BindError
	if !errors.As(err, &bindErr) {
		t.Fatalf("errors.As(*BindError) = false for %v", err)
	}
	if bindErr.Field != "Email" || bindErr.Source != body || bindErr.Name != "email" {
		t.Errorf("BindError = %+v, want Field=Email Source=body Name=email", bindErr)
	}
}

// A request-level failure is not a field error.
func TestRequestErrorsAreNotBindErrors(t *testing.T) {
	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{`))
	r.Header.Set("Content-Type", "application/json")

	var got struct {
		Email string `body:"email"`
	}
	err := Bind(r, &got)

	var bindErr *BindError
	if errors.As(err, &bindErr) {
		t.Errorf("malformed body reported as a field error: %+v", bindErr)
	}
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("got %v, want ErrMalformedBody", err)
	}
}

func TestBindErrorMessageFallbacks(t *testing.T) {
	if got := (&BindError{Message: "explicit"}).Error(); got != "explicit" {
		t.Errorf("Error() = %q, want %q", got, "explicit")
	}
	if got := (&BindError{Err: errors.New("underlying")}).Error(); got != "underlying" {
		t.Errorf("Error() = %q, want %q", got, "underlying")
	}
	if got := (&BindError{}).Error(); got != "bind error" {
		t.Errorf("Error() = %q, want %q", got, "bind error")
	}
}
