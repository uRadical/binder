package binder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Define test structures that match the capabilities of your Bind function
type PathOnlyStruct struct {
	ID int `path:"id"`
}

type QueryOnlyStruct struct {
	Name string `query:"name"`
}

type BodyOnlyStruct struct {
	Email string `body:"email"`
}

type JSONOnlyStruct struct {
	Tags []string `json:"tags"`
}

type CookieOnlyStruct struct {
	SessionID string `cookie:"session_id"`
}

type MixedStruct struct {
	ID        int      `path:"id"`
	Name      string   `query:"name"`
	Email     string   `body:"email"`
	Tags      []string `json:"tags"`
	SessionID string   `cookie:"session_id"`
}

// Helper to set path parameters in a standard way
// Adjust this based on how your Bind function expects to find path parameters
func setPathParams(r *http.Request, params map[string]string) *http.Request {
	// Your Bind function might look for path params in one of these common places:
	// 1. In request context under a specific key
	// 2. In request URL params
	// 3. In a custom request field via type assertion

	// This is a guess - adjust based on your implementation
	return r.WithContext(context.WithValue(r.Context(), "path_params", params))
}

// BenchmarkBindPathOnly benchmarks binding from path parameters only
func BenchmarkBindPathOnly(b *testing.B) {
	// Create a simple request
	r := httptest.NewRequest("GET", "/users/123", nil)

	// Set path parameters - adjust this based on your implementation
	r = setPathParams(r, map[string]string{"id": "123"})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s PathOnlyStruct
		err := Bind(r, &s)
		if err != nil {
			b.Fatalf("Failed to bind path params: %v", err)
		}
	}
}

// BenchmarkBindQueryOnly benchmarks binding from query parameters only
func BenchmarkBindQueryOnly(b *testing.B) {
	// Create a request with query parameters
	r := httptest.NewRequest("GET", "/users?name=test_user", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s QueryOnlyStruct
		err := Bind(r, &s)
		if err != nil {
			b.Fatalf("Failed to bind query params: %v", err)
		}
	}
}

// BenchmarkBindBodyOnly benchmarks binding from form body only
func BenchmarkBindBodyOnly(b *testing.B) {
	b.Run("FormBody", func(b *testing.B) {
		formData := url.Values{}
		formData.Add("email", "test@example.com")
		formBody := formData.Encode()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// Create a new request for each iteration to avoid body read issues
			r := httptest.NewRequest("POST", "/users", strings.NewReader(formBody))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			var s BodyOnlyStruct
			err := Bind(r, &s)
			if err != nil {
				b.Fatalf("Failed to bind form body: %v", err)
			}
		}
	})

	b.Run("JSONBody", func(b *testing.B) {
		jsonBody := `{"tags":["tag1","tag2","tag3"]}`

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// Create a new request for each iteration to avoid body read issues
			r := httptest.NewRequest("POST", "/users", strings.NewReader(jsonBody))
			r.Header.Set("Content-Type", "application/json")

			var s JSONOnlyStruct
			err := Bind(r, &s)
			if err != nil {
				b.Fatalf("Failed to bind JSON body: %v", err)
			}
		}
	})
}

// BenchmarkBindCookieOnly benchmarks binding from cookies only
func BenchmarkBindCookieOnly(b *testing.B) {
	// Create a request with a cookie
	r := httptest.NewRequest("GET", "/users", nil)
	r.AddCookie(&http.Cookie{Name: "session_id", Value: "abc123"})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s CookieOnlyStruct
		err := Bind(r, &s)
		if err != nil {
			b.Fatalf("Failed to bind cookies: %v", err)
		}
	}
}

// BenchmarkBindMixed benchmarks binding from all sources
func BenchmarkBindMixed(b *testing.B) {
	// This is more complex, so we'll handle it differently
	jsonBody := `{"tags":["tag1","tag2","tag3"]}`
	formData := url.Values{}
	formData.Add("email", "test@example.com")

	b.Run("WithJSON", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// Create a fresh request for each iteration
			r := httptest.NewRequest("POST", "/users/123?name=test_user", strings.NewReader(jsonBody))
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(&http.Cookie{Name: "session_id", Value: "abc123"})
			r = setPathParams(r, map[string]string{"id": "123"})

			var s MixedStruct
			err := Bind(r, &s)
			if err != nil {
				b.Fatalf("Failed to bind mixed with JSON: %v", err)
			}
		}
	})

	b.Run("WithForm", func(b *testing.B) {
		formBody := formData.Encode()

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// Create a fresh request for each iteration
			r := httptest.NewRequest("POST", "/users/123?name=test_user", strings.NewReader(formBody))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.AddCookie(&http.Cookie{Name: "session_id", Value: "abc123"})
			r = setPathParams(r, map[string]string{"id": "123"})

			var s MixedStruct
			err := Bind(r, &s)
			if err != nil {
				b.Fatalf("Failed to bind mixed with form: %v", err)
			}
		}
	})
}

// BenchmarkBindOmitEmpty benchmarks binding with omitempty tags
type OmitEmptyStruct struct {
	ID    int    `path:"id,omitempty"`
	Name  string `query:"name,omitempty"`
	Email string `body:"email,omitempty"`
}

func BenchmarkBindOmitEmpty(b *testing.B) {
	// Create a request with just a query parameter
	r := httptest.NewRequest("GET", "/users?name=test_user", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s OmitEmptyStruct
		err := Bind(r, &s)
		if err != nil {
			b.Fatalf("Failed to bind with omitempty: %v", err)
		}
	}
}

// BenchmarkBindParallel benchmarks parallel binding
func BenchmarkBindParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Use a basic form binding as a simple test case
			formData := url.Values{}
			formData.Add("email", "test@example.com")
			formBody := formData.Encode()

			r := httptest.NewRequest("POST", "/users", strings.NewReader(formBody))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			var s BodyOnlyStruct
			err := Bind(r, &s)
			if err != nil {
				b.Fatalf("Failed to bind in parallel: %v", err)
			}
		}
	})
}
