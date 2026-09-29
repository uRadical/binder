package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetStore gives each test the same two users, so tests do not depend on
// the order they run in.
func resetStore(t *testing.T) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	users = map[int]User{
		1: {ID: 1, Name: "Alice", Email: "alice@example.com", Active: true, Tags: []string{"admin", "user"}, CreatedAt: time.Now()},
		2: {ID: 2, Name: "Bob", Email: "bob@example.com", Active: false, Tags: []string{"user"}, CreatedAt: time.Now()},
	}
	nextID = 3
}

// request sends a request through the example's routes and decodes the JSON
// response, if there is one. It reports with Errorf rather than Fatalf so that
// it is safe to call from goroutines.
func request(t *testing.T, method, path, body string, cookies ...*http.Cookie) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	routes().ServeHTTP(w, r)

	var decoded map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
			t.Errorf("%s %s: response is not JSON: %q", method, path, w.Body.String())
		}
	}
	return w.Code, decoded
}

var apiKey = &http.Cookie{Name: "api_key", Value: "demo-key"}

// fieldErrors returns the per-field problems from an error response.
func fieldErrors(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	errs, ok := body["errors"].(map[string]any)
	if !ok {
		t.Fatalf("response %v has no per-field errors", body)
	}
	return errs
}

func TestGetUser(t *testing.T) {
	resetStore(t)

	status, body := request(t, "GET", "/users/1", "")
	if status != http.StatusOK || body["name"] != "Alice" {
		t.Errorf("existing user: got %d %v, want 200 and Alice", status, body)
	}

	if status, _ := request(t, "GET", "/users/99", ""); status != http.StatusNotFound {
		t.Errorf("missing user: got %d, want 404", status)
	}

	status, body = request(t, "GET", "/users/abc", "")
	if status != http.StatusBadRequest {
		t.Fatalf("non-numeric id: got %d, want 400", status)
	}
	if got := fieldErrors(t, body); got["id"] != "invalid value" {
		t.Errorf("non-numeric id: errors = %v, want id: invalid value", got)
	}
}

func TestListUsers(t *testing.T) {
	resetStore(t)

	names := func(body map[string]any) []string {
		var out []string
		for _, u := range body["users"].([]any) {
			out = append(out, u.(map[string]any)["name"].(string))
		}
		return out
	}

	status, body := request(t, "GET", "/users?active=true", "", apiKey)
	if status != http.StatusOK || !reflect.DeepEqual(names(body), []string{"Alice"}) {
		t.Errorf("active filter: got %d %v, want 200 and only Alice", status, body)
	}

	status, body = request(t, "GET", "/users?tags=admin&tags=nobody", "", apiKey)
	if status != http.StatusOK || !reflect.DeepEqual(names(body), []string{"Alice"}) {
		t.Errorf("repeated tags: got %d %v, want 200 and only Alice", status, body)
	}

	if status, _ := request(t, "GET", "/users", ""); status != http.StatusUnauthorized {
		t.Errorf("no API key: got %d, want 401", status)
	}
}

func TestCreateUser(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantFields map[string]any // per-field errors expected, if any
	}{
		{
			name:       "valid",
			body:       `{"name":"Carol","email":"carol@example.com","tags":["user"]}`,
			wantStatus: http.StatusCreated,
		},
		{
			name:       "missing required fields are all reported",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			wantFields: map[string]any{"name": "required", "email": "required"},
		},
		{
			name:       "a bad value and an unknown key together",
			body:       `{"name":"Carol","email":"carol@example.com","active":"maybe","surprise":1}`,
			wantStatus: http.StatusBadRequest,
			wantFields: map[string]any{"active": "invalid value", "surprise": "unknown field"},
		},
		{
			name:       "binds but fails every validation rule",
			body:       `{"name":"Carol","email":"nope","tags":["a","b","c","d","e","f"]}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantFields: map[string]any{"email": "is not a valid address", "tags": "must have at most 5 entries"},
		},
		{
			name:       "malformed JSON",
			body:       `{"name":`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "over the endpoint's 64 KB limit",
			body:       `{"name":"` + strings.Repeat("x", 64<<10) + `"}`,
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetStore(t)

			status, body := request(t, "POST", "/users", tt.body)
			if status != tt.wantStatus {
				t.Fatalf("got %d %v, want %d", status, body, tt.wantStatus)
			}
			if tt.wantFields != nil {
				if got := fieldErrors(t, body); !reflect.DeepEqual(got, tt.wantFields) {
					t.Errorf("errors = %v, want %v", got, tt.wantFields)
				}
			}
			if status == http.StatusCreated && (body["id"] != float64(3) || body["name"] != "Carol") {
				t.Errorf("created %v, want user 3 named Carol", body)
			}
		})
	}
}

// omitempty makes an update partial: fields left out keep their values.
func TestUpdateUser(t *testing.T) {
	resetStore(t)

	status, body := request(t, "PUT", "/users/1", `{"name":"Alicia"}`)
	if status != http.StatusOK {
		t.Fatalf("got %d %v, want 200", status, body)
	}
	if body["name"] != "Alicia" || body["email"] != "alice@example.com" || body["active"] != true {
		t.Errorf("updated %v, want only the name changed", body)
	}

	if status, _ := request(t, "PUT", "/users/99", `{"name":"X"}`); status != http.StatusNotFound {
		t.Errorf("missing user: got %d, want 404", status)
	}
}

func TestDeleteUser(t *testing.T) {
	resetStore(t)

	if status, _ := request(t, "DELETE", "/users/2", ""); status != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204", status)
	}
	if status, _ := request(t, "GET", "/users/2", ""); status != http.StatusNotFound {
		t.Errorf("after delete: got %d, want 404", status)
	}
	if status, _ := request(t, "DELETE", "/users/2", ""); status != http.StatusNotFound {
		t.Errorf("second delete: got %d, want 404", status)
	}
}

// Handlers share an in-memory store and net/http runs them concurrently. Run
// under -race, as CI does, this fails if the store is ever left unguarded.
func TestConcurrentRequests(t *testing.T) {
	resetStore(t)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(4)
		go func() {
			defer wg.Done()
			request(t, "POST", "/users", fmt.Sprintf(`{"name":"U%d","email":"u%d@example.com"}`, i, i))
		}()
		go func() {
			defer wg.Done()
			request(t, "PUT", "/users/1", `{"name":"Z"}`)
		}()
		go func() {
			defer wg.Done()
			request(t, "GET", "/users", "", apiKey)
		}()
		go func() {
			defer wg.Done()
			request(t, "DELETE", fmt.Sprintf("/users/%d", i+3), "")
		}()
	}
	wg.Wait()
}
