package binder

import (
	"io"
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

// setPathParams populates the path values that Bind reads through
// r.PathValue. Values placed anywhere else, the request context included, are
// invisible to it, so a benchmark set up that way binds nothing at all and
// times an empty loop.
func setPathParams(r *http.Request, params map[string]string) *http.Request {
	for name, value := range params {
		r.SetPathValue(name, value)
	}
	return r
}

// resetBody re-arms a request's body between iterations. Rebuilding the whole
// request instead would fold httptest.NewRequest into the measurement, and it
// costs more memory than the binding under test.
func resetBody(r *http.Request, body string) {
	r.Body = io.NopCloser(strings.NewReader(body))
	r.ContentLength = int64(len(body))
}

// requireBound fails a benchmark whose target does not receive the value it
// was set up to bind, so that a broken fixture is reported rather than
// quietly measured.
func requireBound(b *testing.B, field string, got, want interface{}) {
	b.Helper()
	if got != want {
		b.Fatalf("fixture binds nothing: %s = %v, want %v", field, got, want)
	}
}

// BenchmarkBindPathOnly benchmarks binding from path parameters only
func BenchmarkBindPathOnly(b *testing.B) {
	// Create a simple request
	r := httptest.NewRequest("GET", "/users/123", nil)

	// Set path parameters - adjust this based on your implementation
	r = setPathParams(r, map[string]string{"id": "123"})

	var probe PathOnlyStruct
	if err := Bind(r, &probe); err != nil {
		b.Fatalf("Failed to bind path params: %v", err)
	}
	requireBound(b, "ID", probe.ID, 123)

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

	var probe QueryOnlyStruct
	if err := Bind(r, &probe); err != nil {
		b.Fatalf("Failed to bind query params: %v", err)
	}
	requireBound(b, "Name", probe.Name, "test_user")

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

		probeReq := httptest.NewRequest("POST", "/users", strings.NewReader(formBody))
		probeReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		var probe BodyOnlyStruct
		if err := Bind(probeReq, &probe); err != nil {
			b.Fatalf("Failed to bind form body: %v", err)
		}
		requireBound(b, "Email", probe.Email, "test@example.com")

		r := httptest.NewRequest("POST", "/users", strings.NewReader(formBody))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			resetBody(r, formBody)
			r.PostForm = nil // ParseForm caches, so clear it between runs

			var s BodyOnlyStruct
			err := Bind(r, &s)
			if err != nil {
				b.Fatalf("Failed to bind form body: %v", err)
			}
		}
	})

	b.Run("JSONBody", func(b *testing.B) {
		jsonBody := `{"tags":["tag1","tag2","tag3"]}`

		probeReq := httptest.NewRequest("POST", "/users", strings.NewReader(jsonBody))
		probeReq.Header.Set("Content-Type", "application/json")
		var probe JSONOnlyStruct
		if err := Bind(probeReq, &probe); err != nil {
			b.Fatalf("Failed to bind JSON body: %v", err)
		}
		requireBound(b, "len(Tags)", len(probe.Tags), 3)

		r := httptest.NewRequest("POST", "/users", strings.NewReader(jsonBody))
		r.Header.Set("Content-Type", "application/json")

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			resetBody(r, jsonBody)

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

	var probe CookieOnlyStruct
	if err := Bind(r, &probe); err != nil {
		b.Fatalf("Failed to bind cookies: %v", err)
	}
	requireBound(b, "SessionID", probe.SessionID, "abc123")

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
		probeReq := httptest.NewRequest("POST", "/users/123?name=test_user", strings.NewReader(jsonBody))
		probeReq.Header.Set("Content-Type", "application/json")
		probeReq.AddCookie(&http.Cookie{Name: "session_id", Value: "abc123"})
		probeReq = setPathParams(probeReq, map[string]string{"id": "123"})
		var probe MixedStruct
		if err := Bind(probeReq, &probe); err != nil {
			b.Fatalf("Failed to bind mixed with JSON: %v", err)
		}
		requireBound(b, "ID", probe.ID, 123)
		requireBound(b, "Name", probe.Name, "test_user")
		requireBound(b, "SessionID", probe.SessionID, "abc123")
		requireBound(b, "len(Tags)", len(probe.Tags), 3)

		r := httptest.NewRequest("POST", "/users/123?name=test_user", strings.NewReader(jsonBody))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "session_id", Value: "abc123"})
		r = setPathParams(r, map[string]string{"id": "123"})

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			resetBody(r, jsonBody)

			var s MixedStruct
			err := Bind(r, &s)
			if err != nil {
				b.Fatalf("Failed to bind mixed with JSON: %v", err)
			}
		}
	})

	b.Run("WithForm", func(b *testing.B) {
		formBody := formData.Encode()

		probeReq := httptest.NewRequest("POST", "/users/123?name=test_user", strings.NewReader(formBody))
		probeReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		probeReq.AddCookie(&http.Cookie{Name: "session_id", Value: "abc123"})
		probeReq = setPathParams(probeReq, map[string]string{"id": "123"})
		var probe MixedStruct
		if err := Bind(probeReq, &probe); err != nil {
			b.Fatalf("Failed to bind mixed with form: %v", err)
		}
		requireBound(b, "ID", probe.ID, 123)
		requireBound(b, "Name", probe.Name, "test_user")
		requireBound(b, "Email", probe.Email, "test@example.com")
		requireBound(b, "SessionID", probe.SessionID, "abc123")

		r := httptest.NewRequest("POST", "/users/123?name=test_user", strings.NewReader(formBody))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "session_id", Value: "abc123"})
		r = setPathParams(r, map[string]string{"id": "123"})

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			resetBody(r, formBody)
			r.PostForm = nil // ParseForm caches, so clear it between runs

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

	var probe OmitEmptyStruct
	if err := Bind(r, &probe); err != nil {
		b.Fatalf("Failed to bind with omitempty: %v", err)
	}
	requireBound(b, "Name", probe.Name, "test_user")

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
	formData := url.Values{}
	formData.Add("email", "test@example.com")
	formBody := formData.Encode()

	b.RunParallel(func(pb *testing.PB) {
		// One request per goroutine, re-armed per iteration, so the
		// measurement is the binding rather than the fixture.
		r := httptest.NewRequest("POST", "/users", strings.NewReader(formBody))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		for pb.Next() {
			resetBody(r, formBody)
			r.PostForm = nil

			var s BodyOnlyStruct
			err := Bind(r, &s)
			if err != nil {
				b.Fatalf("Failed to bind in parallel: %v", err)
			}
		}
	})
}

// BenchmarkBindManyQueryParams binds several query parameters at once, which
// is where reparsing the query string per field cost the most.
type ManyQueryStruct struct {
	A string `query:"a"`
	B string `query:"b"`
	C string `query:"c"`
	D string `query:"d"`
	E string `query:"e"`
	F string `query:"f"`
	G string `query:"g"`
	H string `query:"h"`
}

func BenchmarkBindManyQueryParams(b *testing.B) {
	r := httptest.NewRequest("GET", "/search?a=1&b=2&c=3&d=4&e=5&f=6&g=7&h=8", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s ManyQueryStruct
		if err := Bind(r, &s); err != nil {
			b.Fatalf("Failed to bind query params: %v", err)
		}
	}
}

// BenchmarkBindNoQueryParams confirms a target that binds from no query
// parameter does not pay to parse the query string.
func BenchmarkBindNoQueryParams(b *testing.B) {
	r := httptest.NewRequest("GET", "/search?a=1&b=2&c=3&d=4&e=5&f=6&g=7&h=8", nil)
	r.AddCookie(&http.Cookie{Name: "session_id", Value: "abc123"})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s CookieOnlyStruct
		if err := Bind(r, &s); err != nil {
			b.Fatalf("Failed to bind cookie: %v", err)
		}
	}
}
