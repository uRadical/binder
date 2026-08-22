package binder

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A required field that is missing from its source must produce an error, and
// the error must name the field so the caller can report it.
func TestRequiredMissingReturnsError(t *testing.T) {
	tests := []struct {
		name  string
		bind  func(*http.Request) error
		field string
	}{
		{
			name:  "body",
			field: "Email",
			bind: func(r *http.Request) error {
				var s struct {
					Email string `body:"email,required"`
				}
				return Bind(r, &s)
			},
		},
		{
			name:  "json",
			field: "Nick",
			bind: func(r *http.Request) error {
				var s struct {
					Nick string `json:"nick,required"`
				}
				return Bind(r, &s)
			},
		},
		{
			name:  "query",
			field: "Page",
			bind: func(r *http.Request) error {
				var s struct {
					Page string `query:"page,required"`
				}
				return Bind(r, &s)
			},
		},
		{
			name:  "cookie",
			field: "Token",
			bind: func(r *http.Request) error {
				var s struct {
					Token string `cookie:"token,required"`
				}
				return Bind(r, &s)
			},
		},
		{
			name:  "path",
			field: "ID",
			bind: func(r *http.Request) error {
				var s struct {
					ID string `path:"id,required"`
				}
				return Bind(r, &s)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"other":"x"}`))
			r.Header.Set("Content-Type", "application/json")

			err := tt.bind(r)
			if err == nil {
				t.Fatalf("missing required %s value: got nil error, want an error", tt.name)
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Errorf("error %q does not name the field %q", err, tt.field)
			}
		})
	}
}

// A required field that is present must bind normally.
func TestRequiredPresentBinds(t *testing.T) {
	type request struct {
		ID    string `path:"id,required"`
		Page  string `query:"page,required"`
		Email string `body:"email,required"`
		Token string `cookie:"token,required"`
	}

	mux := http.NewServeMux()
	var got request
	var bindErr error
	mux.HandleFunc("POST /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		bindErr = Bind(r, &got)
	})

	r := httptest.NewRequest("POST", "/users/42?page=2", strings.NewReader(`{"email":"a@b.c"}`))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: "token", Value: "t0k"})
	mux.ServeHTTP(httptest.NewRecorder(), r)

	if bindErr != nil {
		t.Fatalf("all required values present: got error %v, want nil", bindErr)
	}
	if got != (request{ID: "42", Page: "2", Email: "a@b.c", Token: "t0k"}) {
		t.Errorf("bound %+v, want all four fields populated", got)
	}
}

// An untagged-as-required field that is missing stays at its zero value.
func TestMissingOptionalFieldIsNotAnError(t *testing.T) {
	var s struct {
		Email string `body:"email"`
	}
	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &s); err != nil {
		t.Fatalf("missing optional field: got error %v, want nil", err)
	}
	if s.Email != "" {
		t.Errorf("Email = %q, want empty", s.Email)
	}
}

// A body key present but explicitly empty satisfies required: the value was
// supplied, it is just empty.
func TestRequiredPresentButEmptyBodyValue(t *testing.T) {
	var s struct {
		Email string `body:"email,required"`
	}
	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email":""}`))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &s); err != nil {
		t.Fatalf("required key present but empty: got error %v, want nil", err)
	}
}

// required and omitempty are orthogonal: required fires when the value is
// absent, omitempty when it is present but empty.
func TestRequiredWithOmitEmpty(t *testing.T) {
	type request struct {
		Email string `body:"email,required,omitempty"`
	}

	var absent request
	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	if err := Bind(r, &absent); err == nil {
		t.Error("absent value with required+omitempty: got nil error, want an error")
	}

	var present request
	r = httptest.NewRequest("POST", "/u", strings.NewReader(`{"email":"a@b.c"}`))
	r.Header.Set("Content-Type", "application/json")
	if err := Bind(r, &present); err != nil {
		t.Fatalf("present value with required+omitempty: got error %v, want nil", err)
	}
	if present.Email != "a@b.c" {
		t.Errorf("Email = %q, want %q", present.Email, "a@b.c")
	}
}

func TestHasOption(t *testing.T) {
	tests := []struct {
		opts string
		opt  string
		want bool
	}{
		{"omitempty", optOmitEmpty, true},
		{"required", optRequired, true},
		{"omitempty,required", optRequired, true},
		{"required,omitempty", optOmitEmpty, true},
		{"", optRequired, false},
		{"omitempty", optRequired, false},
		// whole-option matching: substrings must not match
		{"require", optRequired, false},
		{"requiredx", optRequired, false},
		{"notrequired", optRequired, false},
	}
	for _, tt := range tests {
		if got := hasOption(tt.opts, tt.opt); got != tt.want {
			t.Errorf("hasOption(%q, %q) = %v, want %v", tt.opts, tt.opt, got, tt.want)
		}
	}
}
