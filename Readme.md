# Binder - HTTP Request Binding for Go

A focused, zero-dependency library that does one thing well: binding HTTP request data to Go structs. Built for Go 1.27+, using its native path parameter support and standard library UUID type.

## Why Binder?

In REST APIs, you constantly need to extract data from requests - path parameters, query strings, JSON bodies, forms, cookies. Binder handles this tedious work with minimal overhead and maximum clarity.

```go
// Instead of writing this everywhere...
id := r.PathValue("id")
name := r.URL.Query().Get("name")
var body RequestBody
json.NewDecoder(r.Body).Decode(&body)
// ...plus error handling for each

// Just do this:
var req struct {
    ID   int    `path:"id"`
    Name string `query:"name"`
    Body RequestBody `body:"data"`
}
err := binder.Bind(r, &req)
```

## Design Philosophy

**Do one thing, do it well.** Binder only binds data - it doesn't validate, it doesn't log, it doesn't transform. This focused approach means:

- **Zero dependencies** - Just Go's standard library
- **Tiny footprint** - ~600 lines of focused code
- **Fast** - Sub-millisecond binding with caching
- **Predictable** - No magic, no surprises
- **Composable** - Works with your validator, your logger, your framework

## Features

- Bind data from multiple request sources:
  - Path parameters
  - Query parameters
  - JSON request body
  - Form-encoded request body
  - Multipart forms, including file uploads
  - Cookies
  - Request headers
- Support for primitive types, custom types, slices, and nested structs (arrays not supported - use slices)
- Type conversion and validation
- Support for required fields and omitempty behavior
- Custom error handling and reporting

## Installation

```bash
go get uradical.io/go/binder
```

## Quick Start

```go
package main

import (
    "fmt"
    "net/http"
    
    "uradical.io/go/binder"
)

func handler(w http.ResponseWriter, r *http.Request) {
    type UserRequest struct {
        ID        int      `path:"id"`
        Name      string   `query:"name"`
        Email     string   `body:"email"`
        Tags      []string `body:"tags"`
        Newsletter bool    `body:"newsletter,omitempty"`
    }
    
    var req UserRequest
    if err := binder.Bind(r, &req); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    
    fmt.Fprintf(w, "User %d: %s (%s)", req.ID, req.Name, req.Email)
}

func main() {
    mux := http.NewServeMux()
    mux.HandleFunc("POST /users/{id}", handler)
    http.ListenAndServe(":8080", mux)
}
```

## Binding Sources

The library supports binding from multiple sources:

- `path:"name"` - Binds from path parameters (requires a path parameter handler that supports named parameters)
- `query:"name"` - Binds from URL query parameters
- `cookie:"name"` - Binds from HTTP cookies
- `body:"name"` - Binds from request body (form data `x-www-form-urlencoded` or JSON)
- `json:"name"` - Backwards compatibility with existing types
- `header:"name"` - Binds from request headers, matched case-insensitively

Bodies are parsed as JSON, form-encoded data or a multipart form, chosen by the
request's `Content-Type`. The `body:` tag reads from whichever it is.

When a field carries more than one of these, the first in the order above wins:
`path`, `query`, `body`, `json`, `cookie`, `header`.

### Headers

Header names are case-insensitive, so the tag may spell one however it likes:

```go
type Request struct {
    Auth    string `header:"Authorization"`
    TraceID string `header:"x-request-id"`
}
```

### File Uploads

A `multipart/form-data` body binds its text parts like any other body field,
and its file parts to `*multipart.FileHeader`:

```go
type UploadRequest struct {
    Name   string                  `body:"name"`
    Avatar *multipart.FileHeader   `body:"avatar"`
    Docs   []*multipart.FileHeader `body:"docs"`
}

var req UploadRequest
if err := binder.Bind(r, &req); err != nil {
    // Handle binding error
}

f, err := req.Avatar.Open()
```

A field given one file binds a one-element slice; a field declared as a single
file takes the first part sent.

Uploads count against `MaxBodySize` like any other body, and the whole request
is held in memory rather than spilled to a temporary file. Raise the limit
deliberately on an upload endpoint:

```go
binder.BindWithOptions(r, &req, binder.BindOptions{MaxBodySize: 32 << 20})
```

That bound is the point: without one, an upload endpoint is the easiest way to
exhaust a server's memory.

### Repeated Values

A query parameter, header or form field given more than once binds every value
when the destination is a slice, and its first value otherwise:

```go
type Request struct {
    Tags   []string `query:"tags"`   // ?tags=a&tags=b -> ["a", "b"]
    Sort   string   `query:"sort"`   // ?sort=a&sort=b -> "a"
    Accept []string `header:"Accept"`
}
```

A single value still binds as a one-element slice. Values are never split on
commas: `?tags=a,b` is one value, `"a,b"`.

### Body vs JSON Tags

The `body:` tag is the primary tag for binding request body data and automatically handles both JSON and form-encoded 
data based on the request's Content-Type header.

JSON is recognised by media type, including the RFC 6839 suffix form, so
`application/json`, `text/json`, `application/vnd.api+json`,
`application/hal+json` and `application/problem+json` are all parsed as JSON.
A body whose Content-Type is neither JSON nor `application/x-www-form-urlencoded`
is not parsed, and the request binds from its path, query, cookie and header
values alone.

The `json:` tag serves as:
- An alternative to `body:` when working specifically with JSON data
- A way to maintain compatibility with code that already uses `json:` tags for serialization

In most cases, you should prefer using the `body:` tag as it provides content-type awareness.

**Note:** Avoid using both `body:` and `json:` tags on the same field as this creates redundancy.

**Note:** Binder's options travel in whichever tag it reads. On a `json:` tag
that means writing options `encoding/json` does not define, and linters such as
staticcheck will flag `json:"email,required"` as an unknown tag option. Nothing
breaks, but prefer `body:` when a field needs binder options, and keep `json:`
for fields whose tag is shared with serialisation.

## Options

Add `,omitempty` to skip binding if the value is empty:

```go
Email string `body:"email,omitempty"`
```

Add `,required` to return an error if the value is missing from its source:

```go
Email string `body:"email,required"`
```

The error is a `*BindError` wrapping `ErrMissingRequired`. For `path` and
`query` an empty value counts as missing, since neither source distinguishes
the two; a body key that is present but empty satisfies `required`.

## Advanced Usage

### Custom Type Binding

The library supports custom types that implement `encoding.TextUnmarshaler`:

```go
type UserID struct {
    value string
}

func (id *UserID) UnmarshalText(text []byte) error {
    id.value = string(text)
    return nil
}

type Request struct {
    ID UserID `path:"id"`
}
```

### Slices

The library fully supports slices for handling collections of data:

```go
type Request struct {
    Tags     []string  `body:"tags"`
    Scores   []int     `body:"scores"`
    Prices   []float64 `body:"prices"`
}
```

**Note:** Fixed-size arrays (e.g., `[5]int`) are not supported. Always use slices (`[]int`) for collections, as they better match the dynamic nature of REST API data.

### Nested Structs

```go
type Address struct {
    Street string `body:"street"`
    City   string `body:"city"`
}

type User struct {
    Name    string  `body:"name"`
    Address Address `body:"address"`
}
```

### Configuration Options

`BindWithOptions` is `Bind` with per-call configuration. The zero `BindOptions`
behaves exactly as `Bind` does.

```go
opts := binder.BindOptions{
    MaxBodySize:           1 << 20, // 1 MB for this call only
    DisallowUnknownFields: true,    // reject body keys nothing binds
}

if err := binder.BindWithOptions(r, &req, opts); err != nil {
    // Handle error
}
```

| Field | Default | Effect |
|-------|---------|--------|
| `MaxBodySize` | `0` | Overrides the package-level `binder.MaxBodySize` for this call. Zero leaves the package setting in force; a negative value removes the limit for this call alone. |
| `DisallowUnknownFields` | `false` | Fails with `ErrUnknownField` when the body carries a top-level key that no field of the target binds. Keys nested inside objects are not inspected. |

### Request Size Limits

Bodies are capped at `binder.MaxBodySize`, which defaults to 10 MB, so a single
request cannot exhaust server memory. Set it during program initialisation to
change the default for every call:

```go
binder.MaxBodySize = 2 << 20 // 2 MB
```

Set it to zero or less to remove the limit entirely. An oversized body is
rejected with `ErrBodyTooLarge` rather than truncated.

## Error Handling

Failures that concern a single field are of type `*binder.BindError`, which
names the field and the input it came from:

```go
if err := binder.Bind(r, &req); err != nil {
    var bindErr *binder.BindError
    if errors.As(err, &bindErr) {
        fmt.Printf("field %s from %s %q: %v\n",
            bindErr.Field, bindErr.Source, bindErr.Name, bindErr)
    }
    http.Error(w, err.Error(), http.StatusBadRequest)
    return
}
```

Failures that concern the request as a whole are reported with sentinel errors,
so a handler can choose the right status code:

| Error | Meaning | Suggested status |
|-------|---------|------------------|
| `ErrMalformedBody` | The body could not be parsed as its `Content-Type` declares | 400 Bad Request |
| `ErrMissingRequired` | A field tagged `required` had no value; wrapped by a `BindError` | 400 Bad Request |
| `ErrUnknownField` | The body carried a key nothing binds, with `DisallowUnknownFields` set | 400 Bad Request |
| `ErrBodyTooLarge` | The body exceeded `MaxBodySize` | 413 Content Too Large |
| `ErrInvalidTarget` | The target was not a non-nil pointer to a struct | 500 Internal Server Error |

`ErrInvalidTarget` reports a programming error rather than a bad request, so it
is the one case that should not be blamed on the client:

```go
switch {
case errors.Is(err, binder.ErrInvalidTarget):
    http.Error(w, "server error", http.StatusInternalServerError)
case errors.Is(err, binder.ErrBodyTooLarge):
    http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
case err != nil:
    http.Error(w, err.Error(), http.StatusBadRequest)
}
```

## Benchmark Results

Measured on an Apple M-series laptop with Go 1.27, `-benchtime=200ms -count=10`,
reporting the median of ten runs. Reproduce with `make bench`.

Each benchmark times binding alone. The request is built once and its body
re-armed between iterations, so `httptest.NewRequest` is not folded into the
figures; it costs more memory than the binding itself.

| Benchmark | ns/op | B/op | allocs/op |
|-----------|------:|-----:|----------:|
| BindPathOnly | 105 | 72 | 3 |
| BindCookieOnly | 181 | 280 | 5 |
| BindNoQueryParams | 181 | 280 | 5 |
| BindQueryOnly | 227 | 496 | 6 |
| BindOmitEmpty | 250 | 528 | 6 |
| BindParallel | 677 | 2,600 | 25 |
| BindBodyOnly/JSONBody | 890 | 1,824 | 31 |
| BindManyQueryParams | 903 | 832 | 20 |
| BindBodyOnly/FormBody | 1,036 | 2,600 | 25 |
| Bind | 1,250 | 2,152 | 29 |
| BindMixed/WithJSON | 1,302 | 2,544 | 39 |
| BindMixed/WithForm | 1,632 | 3,712 | 36 |
| BindWithoutCache | 1,902 | 3,464 | 35 |
| BindMultipart | 8,850 | 38,069 | 91 |

Binding from path, query, cookie or header costs a few hundred nanoseconds and
a handful of allocations. A JSON body costs more, since the body must be read
and parsed before any field can be converted.

`Bind` against `BindWithoutCache` measures the per-type tag cache: 1,250 ns and
29 allocations with it warm, against 1,882 ns and 35 allocations when it is
cleared before every iteration.

`BindManyQueryParams` binds eight query parameters and `BindNoQueryParams`
binds none, showing that the query string is parsed once per call and only when
a field asks for it.

`BindMultipart` carries two text fields and a 4 KB file. Multipart is an order
of magnitude dearer than the other formats, which is inherent to the encoding
rather than to binding: the parser copies each part, and the file is held in
memory rather than spilled to disk.

## Performance Analysis

* **Fastest binding:** BindPathOnly (0.000 ms/op)
* **Slowest binding:** BindMixed/WithForm (0.004 ms/op)
* **Lowest memory usage:** BindPathOnly (0.01 KB/op)
* **Highest memory usage:** BindMixed/WithForm (9.75 KB/op)
* **Fewest allocations:** BindPathOnly (1 allocs/op)
* **Most allocations:** BindMixed/WithJSON (59 allocs/op)

## Production Ready

This library has been designed with production use in mind:

- **No panics** - An unusable target or an unsettable field is reported, not fatal
- **Bounded reads** - Request bodies are capped, so one request cannot exhaust memory
- **Errors are never swallowed** - A body that fails to parse is reported, not ignored
- **Request body preservation** - Middleware-friendly, allows multiple reads
- **Configurable per call** - `BindWithOptions` avoids reaching for package-level settings
- **Well-tested** - Comprehensive test suite including edge cases

## When to Use Binder

**Perfect for:**
- Standard REST APIs using Go 1.27+
- High-throughput services where performance matters
- Teams that value simplicity and maintainability
- Projects that need to minimize dependencies

**Not suitable for:**
- Complex validation requirements (use a separate validator)
- Older Go versions (requires Go 1.27+)

## Validation

For simple validation your types can implement a Validate function, this will be called as part of the binding:

```go
  type CreateUserRequest struct {
      Name  string `body:"name"`
      Email string `body:"email"`
  }

  func (r CreateUserRequest) Validate() error {
      if r.Name == "" {
          return fmt.Errorf("name is required")
      }
      if r.Email == "" {
          return fmt.Errorf("email is required")
      }
      return nil
  }

  func handler(w http.ResponseWriter, r *http.Request) {
      var req CreateUserRequest

      // Single step: bind + validate
      if err := binder.Bind(r, &req); err != nil {
          http.Error(w, err.Error(), http.StatusBadRequest)
          return
      }

      // Process (req is already validated)
      user := createUser(req)
      json.NewEncoder(w).Encode(user)
  }

```

## Realistic Comparison

This comparison is based on actual analysis of each library's source code:

| Feature | Binder | Echo Binding | Gin Binding | Gorilla Schema |
|---------|--------|--------------|-------------|----------------|
| **Scope** | HTTP→struct binding only | Part of web framework | Part of web framework | Form values only |
| **External Dependencies** | None | None* | validator/v10 | None |
| **Lines of Code** | ~600 | ~500 | ~400 + validator | ~1,400 |
| **Data Sources** | Path, Query, Body, Multipart, Cookie, Header | Path, Query, Body, Header | Path, Query, Body, Header | Query, Form only |
| **Content Types** | JSON, Form | JSON, XML, Form, Multipart | JSON, XML, YAML, TOML, Protobuf, MsgPack | Form only |
| **Built-in Validation** | Interface only | No | Yes (via validator) | No |
| **Native PathValue** | Yes | No | No | N/A |
| **Multipart/Files** | No | Yes | Yes | No |
| **Custom Types** | TextUnmarshaler | BindUnmarshaler | Custom tags | Type converters |
| **Performance** | 0.18-4.76ms | Not benchmarked | Not benchmarked | Not benchmarked |

*Echo framework has dependencies, but the binding package itself uses only standard library

### When to Choose Each:

- **Binder**: You want a standalone, zero-dependency solution for Go 1.27+ REST APIs
- **Echo/Gin**: You're already using these frameworks and want integrated binding
- **Gorilla Schema**: You only need form/query parameter decoding with more features

## Compatibility

Binder follows [Semantic Versioning](https://semver.org/). Within a major
version, the following are stable and will not change incompatibly:

- The exported functions `Bind`, `BindWithOptions` and `BindStruct`.
- The exported types `BindOptions`, `BindError` and `Validator`, and the
  meaning of their fields.
- The sentinel errors `ErrMalformedBody`, `ErrBodyTooLarge`,
  `ErrInvalidTarget`, `ErrMissingRequired` and `ErrUnknownField`. Match on
  these with `errors.Is` rather than on message text.
- The struct tags `path`, `query`, `body`, `json`, `cookie` and `header`, the
  order in which they take precedence, and the `omitempty` and `required`
  options.

The following are **not** part of the contract and may change in any release:

- The text of error messages. Only the sentinels and `BindError`'s fields are
  stable; parsing a message is not supported.
- The default value of `MaxBodySize`. Set it explicitly if your service
  depends on a particular limit.
- The order in which fields are bound, and how many allocations binding takes.

### Go Version Support

Binder requires the two most recent major Go releases. It currently requires
Go 1.27, for the standard library `uuid` package and native path values.
Raising that minimum is a minor version bump, not a major one, in line with
the wider Go ecosystem.

## Contributing

Contributions are welcome! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines on how to contribute to this project.