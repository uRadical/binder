# Binder - HTTP Request Binding for Go

[Docs](https://gobinder.dev) · [API reference](https://pkg.go.dev/uradical.io/go/binder) · [Source](https://github.com/uRadical/binder)

A focused, zero-dependency library that does one thing well: binding HTTP request data to Go structs. Built for Go 1.27+, on `net/http`'s own path parameters.

## Why Binder?

In REST APIs, you constantly need to extract data from requests - path parameters, query strings, JSON bodies, forms, file uploads, cookies and headers. Binder handles this tedious work with minimal overhead and maximum clarity.

```go
// Instead of writing this everywhere...
id, _ := strconv.Atoi(r.PathValue("id"))
name := r.URL.Query().Get("name")
var body struct {
    Email string `json:"email"`
}
json.NewDecoder(r.Body).Decode(&body)
// ...plus error handling for each

// Just do this:
var req struct {
    ID    int    `path:"id"`
    Name  string `query:"name"`
    Email string `body:"email"`
}
err := binder.Bind(r, &req)
```

## Design Philosophy

**Do one thing, do it well.** Binder turns a request into a valid struct: it binds the data, then runs the validation your type defines. It ships no rule language and no validation tags, it doesn't log, and it doesn't transform. This focused approach means:

- **Zero dependencies** - Just Go's standard library
- **Small API** - `Bind`, `BindWithOptions` and a handful of error types
- **Fast and frugal** - Well under a microsecond for a typical request, with as few allocations as the framework binders or fewer, and a fraction of the memory
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
- Support for primitive types, custom types, slices, maps, `any`, nested and embedded structs (arrays not supported - use slices)
- `time.Duration` from text such as `5s` or `1m30s`
- Type conversion
- Validation through your own `Validate(ctx)` method, with the request context available to your rules
- Support for required fields and omitempty behavior
- Every failing field reported at once, as typed errors a handler can inspect

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

- `path:"name"` - Binds from path parameters via `r.PathValue`, as set by `http.ServeMux` patterns such as `/users/{id}` or by any router that calls `r.SetPathValue`
- `query:"name"` - Binds from URL query parameters (a query beyond `net/url`'s limit of 10,000 parameters, like a Cookie header beyond `net/http`'s cookie limit, binds nothing from that source, as those packages do)
- `cookie:"name"` - Binds from HTTP cookies
- `body:"name"` - Binds from the request body: JSON, `x-www-form-urlencoded` or `multipart/form-data`
- `json:"name"` - Backwards compatibility with existing types
- `header:"name"` - Binds from request headers, matched case-insensitively

Bodies are parsed as JSON, form-encoded data or a multipart form, chosen by the
request's `Content-Type`. The `body:` tag reads from whichever it is.

When a field carries more than one of these, the first in this order wins:
`path`, `query`, `body`, `json`, `cookie`, `header`.

A tag with an empty name, such as `query:",required"`, binds under the Go
field name, matched exactly (for a header, case-insensitively, as any header). As in `encoding/json`, `json:"-"` is not a binding
tag.

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

Uploads count against the body size limit like any other body, and the whole request
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

A single value still binds as a one-element slice, and into a slice of
slices as a list of one list; only a recursive type such as `type P []P`, which
would wrap it forever, refuses one. Values are never split on
commas: `?tags=a,b` is one value, `"a,b"`.

### Maps

A map field binds from a JSON object, and from a query string or form body
written as `name[key]=value` pairs, the style OpenAPI calls `deepObject`:

```go
type Search struct {
    Filter map[string]string   `query:"filter"` // ?filter[status]=open&filter[team]=core
    Min    map[string]int      `query:"min"`    // ?min[price]=10
    Flags  map[string]bool     `query:"flag"`   // ?flag[draft]=false
    Tags   map[string][]string `query:"tags"`   // ?tags[any]=a&tags[any]=b
}
```

Keys convert to the map's key type, any type that converts from text: a
string, a number, a bool or a
`TextUnmarshaler`, and values as a field of the element type would, so
`map[string]time.Time` or `map[string]uuid.UUID` work too. In a query or form
a repeated key fills a slice value and otherwise binds its first non-empty
value, and a plain `name=value` with no key is ignored; in a JSON object the
last occurrence binds, as for any member. In a query or form an empty value
counts as absent, and a map with no entries leaves the field as it was, so
`required` reports it; a JSON object binds as sent, so `{}` is an empty map
that satisfies `required`, and a `null` value gives its key the zero value, as
in `encoding/json`. A bad
entry is reported under the name the client sent, such as `min[price]`. Only
one level of brackets is read: `filter[a][b]` has no agreed meaning. The map is
replaced, not merged into. In a multipart form, file parts named this way bind
into a `map[string]*multipart.FileHeader` and are refused by any other map.

### Body vs JSON Tags

The `body:` tag is the primary tag for binding request body data. It handles
JSON, form-encoded and multipart bodies, chosen by the request's Content-Type
header.

JSON is recognised by media type, including the RFC 6839 suffix form, so
`application/json`, `text/json`, `application/vnd.api+json`,
`application/hal+json` and `application/problem+json` are all parsed as JSON.
A body whose Content-Type is none of JSON, `application/x-www-form-urlencoded`
or `multipart/form-data` is not parsed, and the request binds from its path,
query, cookie and header values alone. When a field binds from the body it is still read, within the size limit.

The `json:` tag serves as:
- An alternative to `body:` when working specifically with JSON data
- A way to maintain compatibility with code that already uses `json:` tags for serialization

In most cases, you should prefer using the `body:` tag as it provides content-type awareness.

**Note:** Avoid using both `body:` and `json:` tags on the same field as this creates redundancy.

The `json:` tag follows `encoding/json`'s naming where it matters for a shared
type: `json:"-"` fields are never bound, and an empty name means the field
name. Unlike `encoding/json`, names match exactly rather than
case-insensitively, and binder's own options (`required`, `omitempty`) are read
from the tag.

**Note:** Binder's options travel in whichever tag it reads. On a `json:` tag
that means writing options `encoding/json` does not define, and linters such as
staticcheck will flag `json:"email,required"` as an unknown tag option. Nothing
breaks, but prefer `body:` when a field needs binder options, and keep `json:`
for fields whose tag is shared with serialisation.

## Options

Add `,omitempty` to skip binding if the value is present but empty, leaving
the field as it was. For a nested struct, an empty `{}` is skipped whole, so
the `required` fields inside it are not checked:

```go
Email string `body:"email,omitempty"`
```

A JSON value is empty when it is an empty string, zero, false, null, or an
empty array or object. Every other source carries text, so there only the
empty string is empty. On `path`, `query` and `header` the option changes
nothing, since an empty value there already counts as absent. An absent value
never touches the field, so set defaults on the struct before binding.

On a pointer field `omitempty` has no effect. The pointer already tells a value
that was not sent (nil) from one sent as zero, so `{"active": false}` sets a
`*bool` to false, as a PATCH needs.

Add `,required` to return an error if the value is missing from its source:

```go
Email string `body:"email,required"`
```

The failure is a `*BindError` wrapping `ErrMissingRequired`, reported in
`BindErrors` like any other field failure. For `path`, `query` and `header`
an empty value counts as missing, so `?q=` is treated as no `q`; for a slice,
that holds when every value given is empty, and an empty value among others is
an element, an error for a non-string element type. A body key or
a cookie that is present but empty satisfies `required` (a form map entry
with an empty value counts as absent, as in a query).

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

A type with its own JSON decoding, `UnmarshalJSON` or json/v2's
`UnmarshalJSONFrom`, decodes itself from a JSON body, as with `encoding/json`.
That covers money and decimal types, custom enums and `json.RawMessage`:

```go
type Order struct {
    Total   Money           `body:"total"`   // Money has UnmarshalJSON
    Payload json.RawMessage `body:"payload"` // kept as the client sent it
}
```

A JSON body value is handed over exactly as sent, at any depth; with
`omitempty`, an empty one is skipped instead. A JSON `null` sets nothing (inside a
map, it gives its key the zero value). A JSON string goes to
`UnmarshalText` when the type has that too, so a value reads the same from a
body as from a query string; a type with only `UnmarshalJSON` is given text
from other sources as a JSON string.

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

A nested struct binds from a JSON object, and only its fields with a `body` or
`json` tag bind, as at the top level.

### Embedded Structs

An embedded struct with no tag of its own has its fields promoted, as in
`encoding/json`, so request types can share a set of parameters:

```go
type Paging struct {
    Page  int `query:"page"`
    Limit int `query:"limit"`
}

type ListOrders struct {
    Paging          // ?page=2&limit=20 fills Page and Limit
    *Audit          // allocated only if one of its fields is sent (a null is not)
    Status string `query:"status"`
}
```

Promoted fields bind from every source and take every option. A failure names
the field by its path, such as `Paging.Limit`. An outer field shadows a
promoted one with the same key, `body:` and `json:` counting as the same key,
and at the same depth the first declared wins. An embedded struct tagged with
a name, such as `` Audit `body:"audit"` ``, is an ordinary nested object; one
tagged `` `json:"-"` `` is left out entirely.

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
| `MaxBodySize` | `0` | The largest body, in bytes, this call reads. Zero applies `binder.DefaultMaxBodySize` (10 MB); a negative value removes the limit. |
| `DisallowUnknownFields` | `false` | Reports each top-level body key that no field of the target binds, as a `BindErrors` entry wrapping `ErrUnknownField`. Keys nested inside objects are not inspected. |

### Request Size Limits

`Bind` caps bodies at `binder.DefaultMaxBodySize`, 10 MB, and binding costs
time and memory in proportion to the body, so a single request cannot run a
server out of either. To use a different limit, pass it per call:

```go
binder.BindWithOptions(r, &req, binder.BindOptions{MaxBodySize: 2 << 20}) // 2 MB
```

A `MaxBodySize` of zero applies the default, so setting only another option
never removes the cap. A negative value removes the limit. An oversized body is
rejected with `ErrBodyTooLarge` rather than truncated, and put back whole for
later readers.

A target with no `body` or `json` field does not read the body at all, so it is
neither limited nor parsed, unless `DisallowUnknownFields` is set.

## Error Handling

Binding does not stop at the first bad field. Every field is attempted, and
when any fail, `Bind` returns `binder.BindErrors`, a list of `*BindError`, each
naming the field and the input it came from. It has the same shape for one
failure as for several.

At most 100 failures are reported, in field order. Once binding has recorded
that many in a JSON body it stops converting values and only checks that the rest of the body
is well-formed, so rejecting a flood of bad values costs about what reading it
does; a query or form field stops converting at its own hundredth failure. A
failure path of more than 64 parts, a part being a field or key with its
index, keeps its ends and elides the middle, as `items[0].….orders[3].qty`,
and a map key longer than 64 bytes is named by at most its first 64 and `…`. A
message quotes at most 256 bytes of the value that failed; `Err` keeps it
whole.


```go
if err := binder.Bind(r, &req); err != nil {
    var errs binder.BindErrors
    if errors.As(err, &errs) {
        problems := map[string]string{}
        for _, e := range errs {
            if errors.Is(e, binder.ErrMissingRequired) {
                problems[e.Name] = "required"
            } else {
                problems[e.Name] = "invalid value"
            }
        }
        // respond with problems as a 400 body
        return
    }
    // a request-level failure: see below
}
```

Word client-facing messages from `Name`, `Source` and the sentinel an entry
wraps, as above, rather than from `Message`: its text is not part of the
compatibility promise, and it names Go fields and parser internals.

Entries follow the order the struct declares its fields. A failure inside a
nested struct or a slice is an entry of its own whose `Field` and `Name` give
the path to it, such as `Address.Postcode` and `address.postcode`, or `Tags[2]`
and `tags[2]`. With `DisallowUnknownFields` set, each unknown body member is
an entry too, after the fields, with an empty `Field` and `Err` set to
`ErrUnknownField`.

`errors.Is` sees through the list, so `errors.Is(err, binder.ErrMissingRequired)`
reports whether any field was missing.

A pointer field whose value fails to bind is left nil, so a nil pointer always
means nothing was bound through it. That holds for a pointer to a struct too:
if any of its members fails, the members that bound are dropped with it. A
pointer set before binding is kept. Other fields can be left holding part of a
value that failed, such as the members of a struct that bound, so once `Bind`
has returned an error, act on the failures rather than on the struct.

Failures that concern the request as a whole are reported with sentinel errors
rather than `BindErrors`, so a handler can choose the right status code. They
end binding at once, since nothing bound after them could be trusted:

| Error | Meaning | Suggested status |
|-------|---------|------------------|
| `ErrMalformedBody` | The body could not be parsed as its `Content-Type` declares | 400 Bad Request |
| `ErrBodyTooLarge` | The body exceeded the size limit | 413 Content Too Large |
| `ErrInvalidTarget` | The target was not a non-nil pointer to a struct, the request was nil, or the target has a field that cannot be bound, such as a pointer type that points to itself | 500 Internal Server Error |

Two further sentinels are carried by individual `BindErrors` entries rather
than returned alone: `ErrMissingRequired`, for a field tagged `required` that
had no value, and `ErrUnknownField`. A body that could not be read at all,
such as when a client disconnects mid-upload, also ends binding, and returns the
I/O error wrapped rather than a sentinel.

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
| BindHeaderOnly | 61 | 16 | 1 |
| BindPathOnly | 70 | 8 | 1 |
| BindQueryOnly | 87 | 16 | 1 |
| BindCookieOnly | 94 | 16 | 1 |
| BindNoQueryParams | 94 | 16 | 1 |
| BindOmitEmpty | 123 | 48 | 1 |
| BindParallel | 262 | 552 | 11 |
| BindBodyOnly/JSONBody | 539 | 276 | 11 |
| BindBodyOnly/FormBody | 561 | 552 | 11 |
| BindMixed/WithJSON | 710 | 332 | 11 |
| Bind | 720 | 336 | 7 |
| BindMixed/WithForm | 743 | 616 | 11 |
| BindManyQueryParams | 907 | 128 | 1 |
| BindWithoutCache | 2,104 | 3,409 | 17 |
| BindMultipart | 7,798 | 31,636 | 78 |

The one allocation in the path, query, cookie and header benchmarks is the
target itself escaping to the heap once it is passed as `any`: binding from
those sources allocates nothing of its own. Values are parsed straight into
their fields, and a value without escapes is a substring of the request
rather than a copy. A JSON body costs more, since the body must be read and
parsed before any field can be converted. A form body is parsed into a map
first, and costs about the same as JSON.

`Bind` against `BindWithoutCache` measures the per-type tag cache: 720 ns and
7 allocations with it warm, against 2,104 ns and 17 allocations when it is
cleared before every iteration.

`BindManyQueryParams` binds eight query parameters and `BindNoQueryParams`
binds none. Neither parses the query into `url.Values`: each field's parameter
is found by scanning the raw query, which allocates nothing.

`BindMultipart` carries two text fields and a 4 KB file. Multipart is an order
of magnitude dearer than the other formats, which is inherent to the encoding
rather than to binding: the parser copies each part, and the file is held in
memory rather than spilled to disk.

## Production Ready

This library has been designed with production use in mind:

- **No panics** - An unusable target is reported as an error, and an unexported field is skipped
- **Bounded work** - Bodies are capped, binding costs time and memory in proportion to the body, and at most 100 failures are reported
- **Errors are never swallowed** - A body that fails to parse is reported, not ignored
- **Request body preservation** - The body is restored after binding, so later handlers can read it again
- **Configurable per call** - `BindWithOptions` sets limits per endpoint; there is no package-level state to change
- **Well-tested** - About 93% statement coverage, run under the race detector, with fuzz targets for the reflection paths

## When to Use Binder

**Perfect for:**
- Standard REST APIs using Go 1.27+
- High-throughput services where performance matters
- Teams that value simplicity and maintainability
- Projects that need to minimize dependencies

**Not suitable for:**
- Declarative, tag-driven validation (binder calls your `Validate` method; pair it with a rule library if you want tags)
- Older Go versions (requires Go 1.27+)

## Validation

Your types can implement a `Validate` method, which binder calls once binding succeeds. A validation failure is returned from `Bind` prefixed with `validation failed:`, with your error wrapped so `errors.Is` and `errors.As` still reach it:

```go
type CreateUserRequest struct {
    Name  string `body:"name,required"`
    Email string `body:"email,required"`
    Age   int    `body:"age"`
}

// ValidationErrors is the application's own error type: problems keyed by
// the field name the client sent.
type ValidationErrors map[string]string

func (v ValidationErrors) Error() string {
    return fmt.Sprintf("%d fields failed validation", len(v))
}

func (r CreateUserRequest) Validate(ctx context.Context) error {
    errs := ValidationErrors{}
    if !strings.Contains(r.Email, "@") {
        errs["email"] = "is not a valid address"
    }
    if r.Age < 18 {
        errs["age"] = "must be 18 or older"
    }
    if len(errs) > 0 {
        return errs
    }
    return nil
}

func handler(w http.ResponseWriter, r *http.Request) {
    var req CreateUserRequest
    if err := binder.Bind(r, &req); err != nil {
        var valErrs ValidationErrors
        if errors.As(err, &valErrs) {
            // respond with valErrs, e.g. as a 422 body
            return
        }
        // binding failures: see Error Handling
        return
    }
    // req is bound and validated
}
```

Binder returns what `Validate` returned, wrapped with `%w`, so `errors.Is` and
`errors.As` reach it. The shape is yours. Returning a type of your own, as
above, lets a handler tell validation failures apart with `errors.As`; an
`errors.Join` of plain errors is harder to recognise, since other errors, such
as `ErrMalformedBody`, also wrap more than one error.

`Validate` runs only once every field has bound. A field that failed to bind
holds whatever was left in it, so running your rules over it would add a
second, misleading error for the same input.

A `Validate` promoted from an embedded pointer, such as `*Audit`, is not called
when that pointer is still nil because none of its fields was sent: there is
nothing to validate, and calling it would dereference nil. A type's own
`Validate` always runs, as does one promoted from an embedded value that is not
itself reached through a nil pointer. An embedded `Validator` interface that
is nil is skipped the same way.

Binder passes `r.Context()`, so a rule can use the authenticated user, a
tenant, or the request's deadline for a lookup:

```go
func (r CreateOrderRequest) Validate(ctx context.Context) error {
    user, ok := auth.UserFrom(ctx)
    if !ok || !user.CanOrder(r.SKU) {
        return errors.New("sku not available to this account")
    }
    return nil
}
```

If validation does I/O, a cancelled request surfaces as an error matching
`errors.Is(err, context.Canceled)`.

## Comparison

Features as of Echo v4.15, Gin v1.12 and gorilla/schema v1.4, checked against
their source:

| Feature | Binder | Echo `DefaultBinder` | Gin `binding` | gorilla/schema |
|---------|--------|----------------------|---------------|----------------|
| **Scope** | Standalone binder | Part of the Echo framework | Part of the Gin framework | Decodes `url.Values` only |
| **External dependencies** | None | Echo's | Gin's, including validator/v10 | None |
| **Sources** | Path, query, body, header, cookie | Path, query, body, header | Path, query, body, header | Whatever `url.Values` you pass |
| **Body formats** | JSON, form, multipart | JSON, XML, form, multipart | JSON, XML, form, multipart, YAML, TOML, Protobuf, MsgPack, BSON | N/A |
| **File uploads** | Yes | Yes | Yes | No |
| **Path values** | `http.ServeMux` / `r.PathValue` | Echo's router | Gin's router | N/A |
| **Validation** | Your `Validate(ctx)` method, called by `Bind` | Pluggable `Validator`, called separately via `c.Validate` | validator/v10 tags, called by `ShouldBind` | No |
| **Custom types** | `encoding.TextUnmarshaler`, `UnmarshalJSON` and json/v2's `UnmarshalJSONFrom` | `BindUnmarshaler` and `TextUnmarshaler`, and `UnmarshalJSON` in JSON bodies | `BindUnmarshaler`, `TextUnmarshaler` with a `parser` tag option, and `UnmarshalJSON` in JSON bodies | Registered converters and `TextUnmarshaler` |
| **Reports every bad field** | Yes, as `BindErrors` | No | Validation failures only; conversion stops at the first | Yes, as `MultiError` |

### Speed and Allocations

The [`benchmarks`](benchmarks) module times each library on the same requests,
with a hand-written standard library version as the floor. Medians of ten runs
on an Apple M-series laptop, Go 1.27:

| Scenario | Binder | Echo | Gin | gorilla/schema | Stdlib by hand |
|----------|-------:|-----:|----:|---------------:|---------------:|
| Query string, 5 fields | 523 ns | 881 ns | 1,105 ns | 1,936 ns | 364 ns |
| | 64 B, 1 alloc | 544 B, 8 allocs | 608 B, 9 allocs | 1,367 B, 46 allocs | 480 B, 7 allocs |
| JSON body, 5 fields | 570 ns | 824 ns | 868 ns | - | 745 ns |
| | 256 B, 8 allocs | 681 B, 8 allocs | 681 B, 8 allocs | - | 681 B, 8 allocs |
| Path, query, body, header and cookie | 595 ns | 1,340 ns | 1,473 ns | - | - |
| | 248 B, 6 allocs | 1,202 B, 15 allocs | 1,644 B, 21 allocs | - | - |

Every binding library's figures include 1 allocation for the target escaping
to the heap once it is passed as `any` (the hand-written query version returns
its struct by value and avoids it), and the body benchmarks 2 more for re-arming
the request body each iteration, which no library can avoid. Binder also
restores `r.Body` after reading it, so later handlers can read it again; that
costs it 1 allocation the others don't pay.

The libraries do different amounts of work per call;
`benchmarks/compare_test.go` explains what each benchmark asks of each
library. Reproduce with `cd benchmarks && go test -bench . -benchmem`.

### When to Choose Each

- **Binder**: You use `net/http` and want one call that binds every source, with no dependencies
- **Echo/Gin**: You're already using these frameworks and want integrated binding
- **gorilla/schema**: You only need form or query decoding into structs

## Compatibility

Binder follows [Semantic Versioning](https://semver.org/). Within a major
version, the following are stable and will not change incompatibly:

- The exported functions `Bind` and `BindWithOptions`.
- The exported types `BindOptions`, `BindError`, `BindErrors` and `Validator`,
  and the meaning of their fields.
- The sentinel errors `ErrMalformedBody`, `ErrBodyTooLarge`,
  `ErrInvalidTarget`, `ErrMissingRequired` and `ErrUnknownField`. Match on
  these with `errors.Is` rather than on message text.
- The struct tags `path`, `query`, `body`, `json`, `cookie` and `header`, the
  order in which they take precedence, and the `omitempty` and `required`
  options.

The following are **not** part of the contract and may change in any release:

- The text of error messages. Only the sentinels and `BindError`'s fields are
  stable; parsing a message is not supported.
- The value of `DefaultMaxBodySize`. Pass `BindOptions.MaxBodySize` if your
  service depends on a particular limit.
- The order in which fields are bound, and how many allocations binding takes.

### Go Version Support

Binder supports the Go releases the Go project supports: the two most recent.
It currently requires Go 1.27, so until Go 1.28 ships, 1.27 is the only
supported release. JSON is decoded with `encoding/json/jsontext`, so the
toolchain's jsonv2 experiment must be on, as it is by default;
`GOEXPERIMENT=nojsonv2` will not build binder.
Raising that minimum is a minor version bump, not a major one, in line with
the wider Go ecosystem.

## Contributing

Contributions are welcome! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines on how to contribute to this project.

Binder is maintained by [uRadical](https://uradical.io).
