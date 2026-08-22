# Binder - HTTP Request Binding for Go

A focused, zero-dependency library that does one thing well: binding HTTP request data to Go structs. Built specifically for Go 1.22+ and its native path parameter support.

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
  - Cookies
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

### Body vs JSON Tags

The `body:` tag is the primary tag for binding request body data and automatically handles both JSON and form-encoded 
data based on the request's Content-Type header.

The `json:` tag serves as:
- An alternative to `body:` when working specifically with JSON data
- A way to maintain compatibility with code that already uses `json:` tags for serialization

In most cases, you should prefer using the `body:` tag as it provides content-type awareness.

**Note:** Avoid using both `body:` and `json:` tags on the same field as this creates redundancy.

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

![Benchmark Results](./benchmark_results.png)


| Test | Time (ms/op) | Memory (KB/op) | Allocations |
|------|-------------|---------------|-------------|
| BindPathOnly | 0.000 | 0.01 | 1 |
| BindCookieOnly | 0.000 | 0.23 | 4 |
| BindQueryOnly | 0.000 | 0.44 | 5 |
| BindOmitEmpty | 0.001 | 0.47 | 5 |
| BindParallel | 0.002 | 8.02 | 44 |
| BindBodyOnly/JSONBody | 0.002 | 7.55 | 44 |
| BindBodyOnly/FormBody | 0.002 | 7.87 | 36 |
| Bind | 0.003 | 2.52 | 31 |
| BindWithoutCache | 0.003 | 2.57 | 32 |
| BindMixed/WithJSON | 0.004 | 9.05 | 59 |
| BindMixed/WithForm | 0.004 | 9.75 | 54 |


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
- Standard REST APIs using Go 1.22+
- High-throughput services where performance matters
- Teams that value simplicity and maintainability
- Projects that need to minimize dependencies

**Not suitable for:**
- File uploads (no multipart/form-data support)
- Complex validation requirements (use a separate validator)
- Legacy Go versions (requires Go 1.22+ for path parameters)

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
| **Data Sources** | Path, Query, Body, Cookie | Path, Query, Body, Header | Path, Query, Body, Header | Query, Form only |
| **Content Types** | JSON, Form | JSON, XML, Form, Multipart | JSON, XML, YAML, TOML, Protobuf, MsgPack | Form only |
| **Built-in Validation** | Interface only | No | Yes (via validator) | No |
| **Go 1.22 PathValue** | Yes | No | No | N/A |
| **Multipart/Files** | No | Yes | Yes | No |
| **Custom Types** | TextUnmarshaler | BindUnmarshaler | Custom tags | Type converters |
| **Performance** | 0.18-4.76ms | Not benchmarked | Not benchmarked | Not benchmarked |

*Echo framework has dependencies, but the binding package itself uses only standard library

### When to Choose Each:

- **Binder**: You want a standalone, zero-dependency solution for Go 1.22+ REST APIs
- **Echo/Gin**: You're already using these frameworks and want integrated binding
- **Gorilla Schema**: You only need form/query parameter decoding with more features

## Contributing

Contributions are welcome! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines on how to contribute to this project.