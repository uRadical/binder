package binder

import (
	"encoding"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tag options must be stripped before a tag is used as a lookup key, on every
// source - not just query. See splitTag.
func TestTagOptionsStrippedFromLookupKey(t *testing.T) {
	type request struct {
		ID    string `path:"id,omitempty"`
		Name  string `query:"name,omitempty"`
		Email string `body:"email,omitempty"`
		Nick  string `json:"nick,omitempty"`
		Token string `cookie:"token,omitempty"`
		Plain string `body:"plain"`
	}

	mux := http.NewServeMux()
	var got request
	var bindErr error
	mux.HandleFunc("POST /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		bindErr = Bind(r, &got)
	})

	bodyJSON := `{"email":"a@b.c","nick":"al","plain":"kept"}`
	r := httptest.NewRequest("POST", "/users/42?name=bob", strings.NewReader(bodyJSON))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: "token", Value: "t0k"})
	mux.ServeHTTP(httptest.NewRecorder(), r)

	if bindErr != nil {
		t.Fatalf("Bind returned error: %v", bindErr)
	}

	for _, tc := range []struct{ name, got, want string }{
		{"path", got.ID, "42"},
		{"query", got.Name, "bob"},
		{"body", got.Email, "a@b.c"},
		{"json", got.Nick, "al"},
		{"cookie", got.Token, "t0k"},
		{"no options", got.Plain, "kept"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s source with tag options: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// Tag options must also be stripped for nested struct fields, which are bound
// through a separate code path.
func TestTagOptionsStrippedInNestedStructs(t *testing.T) {
	type address struct {
		City string `body:"city,omitempty"`
		Zip  string `json:"zip,omitempty"`
	}
	type request struct {
		Address address `body:"address"`
	}

	r := httptest.NewRequest("POST", "/u", strings.NewReader(`{"address":{"city":"Dublin","zip":"D01"}}`))
	r.Header.Set("Content-Type", "application/json")

	var got request
	if err := Bind(r, &got); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	if got.Address.City != "Dublin" {
		t.Errorf("nested body tag with options: got %q, want %q", got.Address.City, "Dublin")
	}
	if got.Address.Zip != "D01" {
		t.Errorf("nested json tag with options: got %q, want %q", got.Address.Zip, "D01")
	}
}

func TestSplitTag(t *testing.T) {
	tests := []struct {
		tag      string
		wantName string
		wantOpts string
	}{
		{"email", "email", ""},
		{"email,omitempty", "email", "omitempty"},
		{"email,omitempty,required", "email", "omitempty,required"},
		{",omitempty", "", "omitempty"},
		{"", "", ""},
	}
	for _, tt := range tests {
		name, opts := splitTag(tt.tag)
		if name != tt.wantName || opts != tt.wantOpts {
			t.Errorf("splitTag(%q) = (%q, %q), want (%q, %q)", tt.tag, name, opts, tt.wantName, tt.wantOpts)
		}
	}
}

// As in encoding/json, json:"-" is not a binding tag: it used to bind a body
// key literally named "-". An empty name binds under the Go field name, where
// it used to bind the key "".
func TestJSONTagDashAndEmptyName(t *testing.T) {
	var got struct {
		Secret string `json:"-"`
		Name   string `json:",omitempty"`
		Token  string `json:"-" header:"X-Token"`
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"-":"leaked","Name":"n","":"wrong"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Token", "t")
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	if got.Secret != "" || got.Name != "n" || got.Token != "t" {
		t.Errorf("got Secret=%q Name=%q Token=%q, want \"\", n, t", got.Secret, got.Name, got.Token)
	}
}

// A field whose type is an interface names nothing to bind into. It used to
// panic when that interface was encoding.TextUnmarshaler.
func TestInterfaceFieldIsAnErrorNotAPanic(t *testing.T) {
	var got struct {
		X encoding.TextUnmarshaler `query:"x"`
	}
	err := Bind(httptest.NewRequest("GET", "/?x=1", nil), &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 1 || errs[0].Field != "X" {
		t.Errorf("got %v, want one BindError for X", err)
	}
}
