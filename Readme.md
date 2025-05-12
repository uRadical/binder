# Go HTTP Binder

This lightweight, zero-dependency library is primarily designed to work with Go's standard `net/http` routing for 
binding HTTP request data to Go structs. While it can be integrated with third-party routers that support named path 
parameters, its core functionality is built around the standard library's HTTP handling.

## Features

- Bind data from multiple request sources:
  - Path parameters
  - Query parameters
  - JSON request body
  - Form-encoded request body
  - Cookies
- Support for primitive types, custom types, slices, and nested structs
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

Add `,required` to return an error if the field is missing:

```go
Email string `body:"email,required"`
```

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

```go
opts := binder.BindOptions{
    SkipUnknownFields: true,
    DisallowExtraFields: false,
    ErrorOnRequired: true,
}

if err := binder.BindWithOptions(r, &req, opts); err != nil {
    // Handle error
}
```

## Error Handling

Errors from binding are of type `*binder.BindError`, which provides detailed information about what went wrong:

```go
if err := binder.Bind(r, &req); err != nil {
    if bindErr, ok := err.(*binder.BindError); ok {
        fmt.Printf("Error binding field '%s': %s\n", bindErr.Field, bindErr.Message)
    }
    http.Error(w, err.Error(), http.StatusBadRequest)
    return
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

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.