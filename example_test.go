// Examples are in an external test package so they use the library exactly as
// a caller does, and so anything they need is provably exported.
package binder_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"uradical.io/go/binder"
)

func ExampleBind() {
	type CreateUser struct {
		Name  string   `body:"name"`
		Email string   `body:"email"`
		Tags  []string `body:"tags"`
	}

	r := httptest.NewRequest("POST", "/users",
		strings.NewReader(`{"name":"Alice","email":"alice@example.com","tags":["admin","user"]}`))
	r.Header.Set("Content-Type", "application/json")

	var req CreateUser
	if err := binder.Bind(r, &req); err != nil {
		fmt.Println("bind failed:", err)
		return
	}

	fmt.Println(req.Name, req.Email, req.Tags)
	// Output: Alice alice@example.com [admin user]
}

// Bind draws from several parts of a request at once.
func ExampleBind_multipleSources() {
	type UpdateUser struct {
		ID      int    `path:"id"`
		Notify  string `query:"notify"`
		Email   string `body:"email"`
		Session string `cookie:"session"`
		TraceID string `header:"X-Request-ID"`
	}

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req UpdateUser
		if err := binder.Bind(r, &req); err != nil {
			fmt.Println("bind failed:", err)
			return
		}
		fmt.Printf("%d %s %s %s %s\n", req.ID, req.Notify, req.Email, req.Session, req.TraceID)
	})

	r := httptest.NewRequest("PUT", "/users/42?notify=yes", strings.NewReader(`{"email":"a@b.c"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Request-ID", "trace-1")
	r.AddCookie(&http.Cookie{Name: "session", Value: "s3ss"})
	mux.ServeHTTP(httptest.NewRecorder(), r)

	// Output: 42 yes a@b.c s3ss trace-1
}

// The required option reports a value missing from its source.
func ExampleBind_required() {
	type CreateUser struct {
		Email string `body:"email,required"`
	}

	r := httptest.NewRequest("POST", "/users", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")

	var req CreateUser
	err := binder.Bind(r, &req)
	fmt.Println(err)
	fmt.Println(errors.Is(err, binder.ErrMissingRequired))
	// Output:
	// missing required field Email: no body value named "email"
	// true
}

// The omitempty option leaves a field alone when the value is present but
// empty, which suits partial updates.
func ExampleBind_omitEmpty() {
	type PatchUser struct {
		Name string `body:"name,omitempty"`
	}

	req := PatchUser{Name: "existing"}
	r := httptest.NewRequest("PATCH", "/users/1", strings.NewReader(`{"name":""}`))
	r.Header.Set("Content-Type", "application/json")

	if err := binder.Bind(r, &req); err != nil {
		fmt.Println("bind failed:", err)
		return
	}
	fmt.Println(req.Name)
	// Output: existing
}

// A parameter given more than once fills a slice.
func ExampleBind_repeatedValues() {
	type Search struct {
		Tags []string `query:"tag"`
		Sort string   `query:"sort"`
	}

	r := httptest.NewRequest("GET", "/search?tag=go&tag=http&sort=name", nil)

	var req Search
	if err := binder.Bind(r, &req); err != nil {
		fmt.Println("bind failed:", err)
		return
	}
	fmt.Println(req.Tags, req.Sort)
	// Output: [go http] name
}

func ExampleBindWithOptions() {
	type CreateUser struct {
		Email string `body:"email"`
	}

	r := httptest.NewRequest("POST", "/users",
		strings.NewReader(`{"email":"a@b.c","typo":1}`))
	r.Header.Set("Content-Type", "application/json")

	var req CreateUser
	err := binder.BindWithOptions(r, &req, binder.BindOptions{
		MaxBodySize:           64 << 10,
		DisallowUnknownFields: true,
	})

	fmt.Println(err)
	fmt.Println(errors.Is(err, binder.ErrUnknownField))
	// Output:
	// unknown field in request body: "typo"
	// true
}

// BindError identifies which input was at fault, so a handler can say more
// than that something went wrong.
func ExampleBindError() {
	type Search struct {
		Limit int `query:"limit"`
	}

	r := httptest.NewRequest("GET", "/search?limit=lots", nil)

	var req Search
	err := binder.Bind(r, &req)

	var bindErr *binder.BindError
	if errors.As(err, &bindErr) {
		fmt.Printf("field %s came from %s %q\n", bindErr.Field, bindErr.Source, bindErr.Name)
	}
	// Output: field Limit came from query "limit"
}

// A type implementing Validator is checked after binding succeeds.
func ExampleValidator() {
	r := httptest.NewRequest("POST", "/users", strings.NewReader(`{"age":15}`))
	r.Header.Set("Content-Type", "application/json")

	var req signup
	err := binder.Bind(r, &req)
	fmt.Println(err)
	// Output: validation failed: must be 18 or older
}

type signup struct {
	Age int `body:"age"`
}

func (s signup) Validate() error {
	if s.Age < 18 {
		return errors.New("must be 18 or older")
	}
	return nil
}

// Failures that concern the request as a whole are reported with sentinel
// errors, so a handler can pick the right status code.
func ExampleBind_errorHandling() {
	type CreateUser struct {
		Email string `body:"email"`
	}

	r := httptest.NewRequest("POST", "/users", strings.NewReader(`{"email": NOT JSON`))
	r.Header.Set("Content-Type", "application/json")

	var req CreateUser
	err := binder.Bind(r, &req)

	// Match on the sentinels rather than on message text, which is not part
	// of the compatibility promise.
	switch {
	case errors.Is(err, binder.ErrInvalidTarget):
		fmt.Println(http.StatusInternalServerError)
	case errors.Is(err, binder.ErrBodyTooLarge):
		fmt.Println(http.StatusRequestEntityTooLarge)
	case errors.Is(err, binder.ErrMalformedBody):
		fmt.Println(http.StatusBadRequest, "malformed body")
	case err != nil:
		fmt.Println(http.StatusBadRequest, err)
	}
	// Output: 400 malformed body
}
