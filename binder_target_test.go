package binder

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type bindTarget struct {
	Email string `body:"email"`
}

func jsonRequest() *http.Request {
	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email":"a@b.c"}`))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// Bind must report an unusable target instead of panicking inside reflect.
func TestInvalidTargetReturnsError(t *testing.T) {
	pointer := &bindTarget{}
	number := 0
	mapping := map[string]string{}

	tests := []struct {
		name   string
		target interface{}
	}{
		{"nil interface", nil},
		{"non-pointer struct", bindTarget{}},
		{"typed nil pointer", (*bindTarget)(nil)},
		{"pointer to int", &number},
		{"pointer to map", &mapping},
		{"pointer to pointer", &pointer},
		{"plain string", "not a target"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("Bind panicked instead of returning an error: %v", p)
				}
			}()

			err := Bind(jsonRequest(), tt.target)
			if err == nil {
				t.Fatal("got nil error, want ErrInvalidTarget")
			}
			if !errors.Is(err, ErrInvalidTarget) {
				t.Errorf("errors.Is(err, ErrInvalidTarget) = false for %v", err)
			}
		})
	}
}

func TestNilRequestReturnsError(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Bind panicked on a nil request: %v", p)
		}
	}()

	var got bindTarget
	err := Bind(nil, &got)
	if !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("got %v, want ErrInvalidTarget", err)
	}
}

// A tagged unexported field is ignored rather than panicking, as with
// encoding/json. Exported fields around it still bind.
func TestUnexportedTaggedFieldIgnored(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Bind panicked on an unexported tagged field: %v", p)
		}
	}()

	var got struct {
		email string `body:"email"`
		Name  string `body:"name"`
	}

	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"email":"a@b.c","name":"al"}`))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Name != "al" {
		t.Errorf("Name = %q, want %q - exported fields must still bind", got.Name, "al")
	}
	if got.email != "" {
		t.Errorf("email = %q, want empty", got.email)
	}
}

// An unexported field tagged required must not resurrect the panic through the
// required check either.
func TestUnexportedRequiredFieldIgnored(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Bind panicked: %v", p)
		}
	}()

	var got struct {
		email string `body:"email,required"`
	}
	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil - an unsettable field is not bound at all", err)
	}
}

// A valid target still binds, and the error names the offending type.
func TestValidTargetUnaffected(t *testing.T) {
	var got bindTarget
	if err := Bind(jsonRequest(), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Email != "a@b.c" {
		t.Errorf("Email = %q, want %q", got.Email, "a@b.c")
	}
}

func TestInvalidTargetErrorNamesType(t *testing.T) {
	number := 0
	err := Bind(jsonRequest(), &number)
	if err == nil {
		t.Fatal("got nil error")
	}
	if !strings.Contains(err.Error(), "*int") {
		t.Errorf("error %q does not name the offending type", err)
	}
}
