// Package benchmarks compares binder against other Go request binders.
//
// Reading these fairly needs three caveats:
//
//   - The libraries do not do the same amount of work. Binder reads every
//     source in one call; Echo's binder reads path, query and body; Gin needs
//     a separate call per source; gorilla/schema decodes a url.Values and
//     nothing else. Each benchmark says what it asked of each.
//   - Framework setup is kept out of the timed loop wherever the library
//     allows a context to be reused, since it is not binding. Echo caches the
//     parsed query on its context and SetRequest does not clear it, so the
//     loop calls Reset as Echo's own server does per request. Without that,
//     Echo parses the query once for the whole benchmark and reports a figure
//     no real server would see.
//   - A hand-written stdlib version is included as the floor. Every reflective
//     binder is slower than reading fields by name; the question is by how
//     much, and what that buys.
package benchmarks

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/schema"
	"github.com/labstack/echo/v4"
	"uradical.io/go/binder"
)

func init() { gin.SetMode(gin.ReleaseMode) }

// ---------------------------------------------------------------- fixtures

const queryString = "name=alice&age=30&active=true&city=dublin&sort=name"
const jsonBody = `{"name":"alice","age":30,"active":true,"city":"dublin","sort":"name"}`

type binderQuery struct {
	Name   string `query:"name"`
	Age    int    `query:"age"`
	Active bool   `query:"active"`
	City   string `query:"city"`
	Sort   string `query:"sort"`
}

type echoQuery struct {
	Name   string `query:"name"`
	Age    int    `query:"age"`
	Active bool   `query:"active"`
	City   string `query:"city"`
	Sort   string `query:"sort"`
}

type ginQuery struct {
	Name   string `form:"name"`
	Age    int    `form:"age"`
	Active bool   `form:"active"`
	City   string `form:"city"`
	Sort   string `form:"sort"`
}

type schemaQuery struct {
	Name   string `schema:"name"`
	Age    int    `schema:"age"`
	Active bool   `schema:"active"`
	City   string `schema:"city"`
	Sort   string `schema:"sort"`
}

type binderBody struct {
	Name   string `body:"name"`
	Age    int    `body:"age"`
	Active bool   `body:"active"`
	City   string `body:"city"`
	Sort   string `body:"sort"`
}

type jsonTagged struct {
	Name   string `json:"name"`
	Age    int    `json:"age"`
	Active bool   `json:"active"`
	City   string `json:"city"`
	Sort   string `json:"sort"`
}

// check fails the benchmark unless the fixture actually bound, so that a
// misconfigured comparison is reported rather than timed.
func check(b *testing.B, label string, name string, age int, active bool) {
	b.Helper()
	if name != "alice" || age != 30 || !active {
		b.Fatalf("%s bound nothing usable: name=%q age=%d active=%v", label, name, age, active)
	}
}

func queryRequest() *http.Request {
	return httptest.NewRequest("GET", "/search?"+queryString, nil)
}

func jsonRequest() *http.Request {
	r := httptest.NewRequest("POST", "/users", strings.NewReader(jsonBody))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// ------------------------------------------------- query string, 5 fields

func BenchmarkQuery_Binder(b *testing.B) {
	r := queryRequest()

	var probe binderQuery
	if err := binder.Bind(r, &probe); err != nil {
		b.Fatal(err)
	}
	check(b, "binder", probe.Name, probe.Age, probe.Active)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s binderQuery
		if err := binder.Bind(r, &s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQuery_Echo(b *testing.B) {
	e := echo.New()
	r := queryRequest()
	c := e.NewContext(r, httptest.NewRecorder())

	var probe echoQuery
	if err := c.Bind(&probe); err != nil {
		b.Fatal(err)
	}
	check(b, "echo", probe.Name, probe.Age, probe.Active)

	rec := httptest.NewRecorder()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Reset is what Echo's server calls per request; it clears the
		// cached query so each iteration parses it as a real request would.
		c.Reset(r, rec)

		var s echoQuery
		if err := c.Bind(&s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQuery_Gin(b *testing.B) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = queryRequest()

	var probe ginQuery
	if err := c.ShouldBindQuery(&probe); err != nil {
		b.Fatal(err)
	}
	check(b, "gin", probe.Name, probe.Age, probe.Active)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s ginQuery
		if err := c.ShouldBindQuery(&s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQuery_GorillaSchema(b *testing.B) {
	dec := schema.NewDecoder()
	r := queryRequest()
	values := r.URL.Query()

	var probe schemaQuery
	if err := dec.Decode(&probe, values); err != nil {
		b.Fatal(err)
	}
	check(b, "gorilla/schema", probe.Name, probe.Age, probe.Active)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s schemaQuery
		// The caller must parse the query itself; that cost is included,
		// since the other libraries do it internally.
		if err := dec.Decode(&s, r.URL.Query()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQuery_Stdlib(b *testing.B) {
	r := queryRequest()

	bind := func(r *http.Request) (binderQuery, error) {
		q := r.URL.Query()
		age, err := strconv.Atoi(q.Get("age"))
		if err != nil {
			return binderQuery{}, err
		}
		active, err := strconv.ParseBool(q.Get("active"))
		if err != nil {
			return binderQuery{}, err
		}
		return binderQuery{
			Name: q.Get("name"), Age: age, Active: active,
			City: q.Get("city"), Sort: q.Get("sort"),
		}, nil
	}

	probe, err := bind(r)
	if err != nil {
		b.Fatal(err)
	}
	check(b, "stdlib", probe.Name, probe.Age, probe.Active)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := bind(r); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------- JSON body, 5 fields

func resetBody(r *http.Request, body string) {
	r.Body = io.NopCloser(strings.NewReader(body))
	r.ContentLength = int64(len(body))
}

func BenchmarkJSON_Binder(b *testing.B) {
	r := jsonRequest()

	var probe binderBody
	if err := binder.Bind(r, &probe); err != nil {
		b.Fatal(err)
	}
	check(b, "binder", probe.Name, probe.Age, probe.Active)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetBody(r, jsonBody)
		var s binderBody
		if err := binder.Bind(r, &s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJSON_Echo(b *testing.B) {
	e := echo.New()
	r := jsonRequest()
	rec := httptest.NewRecorder()
	c := e.NewContext(r, rec)

	var probe jsonTagged
	if err := c.Bind(&probe); err != nil {
		b.Fatal(err)
	}
	check(b, "echo", probe.Name, probe.Age, probe.Active)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetBody(r, jsonBody)
		c.Reset(r, rec)

		var s jsonTagged
		if err := c.Bind(&s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJSON_Gin(b *testing.B) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = jsonRequest()

	var probe jsonTagged
	if err := c.ShouldBindJSON(&probe); err != nil {
		b.Fatal(err)
	}
	check(b, "gin", probe.Name, probe.Age, probe.Active)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetBody(c.Request, jsonBody)
		var s jsonTagged
		if err := c.ShouldBindJSON(&s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJSON_Stdlib(b *testing.B) {
	r := jsonRequest()

	var probe jsonTagged
	if err := json.NewDecoder(r.Body).Decode(&probe); err != nil {
		b.Fatal(err)
	}
	check(b, "stdlib", probe.Name, probe.Age, probe.Active)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetBody(r, jsonBody)
		var s jsonTagged
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			b.Fatal(err)
		}
	}
}

// --------------------------------------- all sources at once, as a handler
//
// This is what binder is built for and what the others are not: filling one
// struct from path, query, body, header and cookie. Binder does it in a
// single call; Echo and Gin need one call per source plus manual reads for
// the sources they do not bind, so the comparison is of the whole job rather
// than of one method.

const mixedQuery = "sort=name"
const mixedBody = `{"email":"alice@example.com","age":30}`

type binderMixed struct {
	ID      int    `path:"id"`
	Sort    string `query:"sort"`
	Email   string `body:"email"`
	Age     int    `body:"age"`
	Trace   string `header:"X-Request-ID"`
	Session string `cookie:"session"`
}

type echoMixed struct {
	ID    int    `param:"id"`
	Sort  string `query:"sort"`
	Email string `json:"email"`
	Age   int    `json:"age"`
}

type ginMixedURI struct {
	ID int `uri:"id"`
}
type ginMixedQuery struct {
	Sort string `form:"sort"`
}
type ginMixedBody struct {
	Email string `json:"email"`
	Age   int    `json:"age"`
}

func mixedRequest() *http.Request {
	r := httptest.NewRequest("POST", "/users/42?"+mixedQuery, strings.NewReader(mixedBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Request-ID", "trace-1")
	r.AddCookie(&http.Cookie{Name: "session", Value: "s3ss"})
	return r
}

func checkMixed(b *testing.B, label, email string, id, age int, trace, session string) {
	b.Helper()
	if email != "alice@example.com" || id != 42 || age != 30 || trace != "trace-1" || session != "s3ss" {
		b.Fatalf("%s did not fill the struct: id=%d email=%q age=%d trace=%q session=%q",
			label, id, email, age, trace, session)
	}
}

func BenchmarkMixed_Binder(b *testing.B) {
	// SetPathValue populates what Bind reads, so no router is needed and the
	// request can be reused, keeping construction out of the measurement.
	r := mixedRequest()
	r.SetPathValue("id", "42")

	var probe binderMixed
	if err := binder.Bind(r, &probe); err != nil {
		b.Fatal(err)
	}
	checkMixed(b, "binder", probe.Email, probe.ID, probe.Age, probe.Trace, probe.Session)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetBody(r, mixedBody)

		var s binderMixed
		if err := binder.Bind(r, &s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMixed_Echo(b *testing.B) {
	e := echo.New()
	rec := httptest.NewRecorder()
	r := mixedRequest()
	c := e.NewContext(r, rec)
	queryBinder := &echo.DefaultBinder{}

	// Echo's binder reads path and body here; on a POST the query has to be
	// asked for separately, and header and cookie are read by hand.
	bind := func() (echoMixed, string, string, error) {
		resetBody(r, mixedBody)
		c.Reset(r, rec)
		c.SetParamNames("id")
		c.SetParamValues("42")

		var s echoMixed
		if err := c.Bind(&s); err != nil {
			return s, "", "", err
		}
		if err := queryBinder.BindQueryParams(c, &s); err != nil {
			return s, "", "", err
		}
		trace := c.Request().Header.Get("X-Request-ID")
		var session string
		if ck, err := c.Cookie("session"); err == nil {
			session = ck.Value
		}
		return s, trace, session, nil
	}

	probe, trace, session, err := bind()
	if err != nil {
		b.Fatal(err)
	}
	checkMixed(b, "echo", probe.Email, probe.ID, probe.Age, trace, session)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := bind(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMixed_Gin(b *testing.B) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	r := mixedRequest()

	bind := func() (ginMixedURI, ginMixedQuery, ginMixedBody, string, string, error) {
		resetBody(r, mixedBody)
		c.Request = r
		c.Params = gin.Params{{Key: "id", Value: "42"}}

		var uri ginMixedURI
		var query ginMixedQuery
		var body ginMixedBody
		if err := c.ShouldBindUri(&uri); err != nil {
			return uri, query, body, "", "", err
		}
		if err := c.ShouldBindQuery(&query); err != nil {
			return uri, query, body, "", "", err
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			return uri, query, body, "", "", err
		}
		trace := r.Header.Get("X-Request-ID")
		session, _ := c.Cookie("session")
		return uri, query, body, trace, session, nil
	}

	uri, _, body, trace, session, err := bind()
	if err != nil {
		b.Fatal(err)
	}
	checkMixed(b, "gin", body.Email, uri.ID, body.Age, trace, session)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, _, _, err := bind(); err != nil {
			b.Fatal(err)
		}
	}
}
