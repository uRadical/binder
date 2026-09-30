# Binder Example - REST API Server

This example demonstrates how to use the Binder library to build a complete REST API server with all common binding scenarios.

## What This Example Shows

- **Path parameters** - `/users/{id}`
- **Query parameters** - `/users?active=true&limit=5`
- **Request bodies** - JSON data in POST/PUT requests
- **Cookies** - API key authentication
- **Headers** - Request tracing via `X-Request-ID`
- **Repeated values** - `/users?tags=admin&tags=user` filling a slice
- **UUIDs** - A `uuid.UUID` bound from a JSON body and a query string
- **Embedded structs** - A shared `Paging` struct whose fields bind as if declared on the request
- **Required fields** - Reporting a missing value rather than binding a zero one
- **Per-call options** - `BindWithOptions` for body limits and unknown fields
- **Validation** - Using the `Validator` interface
- **Partial updates** - Using `omitempty` for PATCH-like behavior
- **Error handling** - One function turning every binding error into a response

## Running the Example

```bash
# From the binder repository root
cd example
go run .
```

The server will start on `http://localhost:8080`

`main_test.go` exercises every endpoint and error case below through the same
routes, and checks the shared store under concurrent requests:

```bash
go test -race .
```

## API Endpoints

### 1. Get User by ID
```bash
# Demonstrates path parameter binding
curl http://localhost:8080/users/1
```

**Binder features:**
- `path:"id"` - Extracts user ID from URL path

### 2. List Users with Filters
```bash
# Demonstrates query parameters, repeated values and cookies. The list needs
# the api_key cookie: a browser gets it from its first response, curl needs -b.
curl -b api_key=demo-key 'http://localhost:8080/users?active=true&limit=5'
curl -b api_key=demo-key 'http://localhost:8080/users?page=2&limit=1'
curl -b api_key=demo-key 'http://localhost:8080/users?team=0192f4a0-7b3c-7d4e-9a1b-2c3d4e5f6a70'
```

**Binder features:**
- `query:"active"` - Optional boolean filter into a `*bool`, nil when not given
- `Paging` embedded - its `query:"page"` and `query:"limit"` fields are promoted, so any list endpoint gets paging by embedding one struct
- `query:"team"` - Optional team filter into a `*uuid.UUID`, nil when not given
- `cookie:"api_key"` - API key from cookie; the demo middleware sets it on the response, so a browser sends it from the second request on

### 3. Create User
```bash
# Demonstrates JSON body binding and validation
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Charlie",
    "email": "charlie@example.com",
    "active": true,
    "tags": ["user", "premium"],
    "team_id": "0192f4a0-7b3c-7d4e-9a1b-2c3d4e5f6a70"
  }'
```

**Binder features:**
- `body:"name"` - Required field from JSON
- `body:"email"` - Required field from JSON
- `body:"active"` - Boolean from JSON
- `body:"tags"` - Slice of strings from JSON
- `body:"team_id"` - A JSON string parsed into a `uuid.UUID`
- `Validate(ctx)` method - Custom validation after binding

### 4. Update User (Partial)
```bash
# Demonstrates combining path and body binding with partial updates
curl -X PUT http://localhost:8080/users/1 \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Alice Updated",
    "active": false
  }'
```

**Binder features:**
- `path:"id"` - User ID from URL
- `body:"name,omitempty"` - Optional update to name
- `body:"active"` - Optional update to active status, into a `*bool` so that `false` is an update and leaving it out is not
- Pointer fields (`*bool`) - Distinguish between false and not provided

### 5. Delete User
```bash
# Demonstrates path parameter binding
curl -X DELETE http://localhost:8080/users/1
```

## Key Concepts Demonstrated

### Request Binding Structure
```go
type UpdateUserRequest struct {
    ID     int      `path:"id"`                // From URL path
    Name   string   `body:"name,omitempty"`    // From JSON body, optional
    Email  string   `body:"email,omitempty"`   // From JSON body, optional
    Active *bool    `body:"active"`            // Pointer: nil when absent, so false still updates
    Tags   []string `body:"tags,omitempty"`    // Slice from JSON array
}
```

### Required Fields and Validation

Presence is a binding concern, so the `required` option handles it. Validation
is left for rules binding cannot express:

```go
type CreateUserRequest struct {
    Name  string `body:"name,required"`
    Email string `body:"email,required"`
    // ... other fields
}

// ValidationErrors is the application's own error type: problems keyed by
// the field name the client sent.
type ValidationErrors map[string]string

func (v ValidationErrors) Error() string {
    return fmt.Sprintf("%d fields failed validation", len(v))
}

// Implement binder.Validator interface, checking every rule
func (r CreateUserRequest) Validate(ctx context.Context) error {
    errs := ValidationErrors{}
    if !strings.Contains(r.Email, "@") {
        errs["email"] = "is not a valid address"
    }
    if len(r.Tags) > 5 {
        errs["tags"] = "must have at most 5 entries"
    }
    if len(errs) > 0 {
        return errs
    }
    return nil
}
```

What `Validate` returns is yours. Binder hands it back wrapped, so returning a
type of your own lets `writeBindError` recognise it with `errors.As`.

### Error Handling Pattern

Handlers call `binder.Bind` directly and pass any error to `writeBindError`,
which an application writes once. Binder reports what went wrong as types to
match; the response format and status codes are the application's choice:

```go
func createUser(w http.ResponseWriter, r *http.Request) {
    var req CreateUserRequest
    if err := binder.Bind(r, &req); err != nil {
        writeBindError(w, err)
        return
    }
    // ...
}

func writeBindError(w http.ResponseWriter, err error) {
    var bindErrs binder.BindErrors
    var valErrs ValidationErrors

    switch {
    case errors.As(err, &bindErrs):
        // Every field that failed, keyed by the name the client sent.
        fields := map[string]string{}
        for _, e := range bindErrs {
            switch {
            case errors.Is(e, binder.ErrMissingRequired):
                fields[e.Name] = "required"
            case errors.Is(e, binder.ErrUnknownField):
                fields[e.Name] = "unknown field"
            default:
                fields[e.Name] = "invalid value"
            }
        }
        respondJSON(w, map[string]any{"errors": fields}, http.StatusBadRequest)

    case errors.As(err, &valErrs):
        respondJSON(w, map[string]any{"errors": valErrs}, http.StatusUnprocessableEntity)

    case errors.Is(err, binder.ErrBodyTooLarge):
        respondError(w, "request body too large", http.StatusRequestEntityTooLarge)

    case errors.Is(err, binder.ErrInvalidTarget):
        // A bug in this handler, not a bad request.
        respondError(w, "internal server error", http.StatusInternalServerError)

    default:
        respondError(w, err.Error(), http.StatusBadRequest)
    }
}
```

### Per-Call Options

`BindWithOptions` narrows the rules for one endpoint. Creating a user takes a
tighter body limit than the package default and refuses keys nothing binds, so
a typo is reported instead of ignored:

```go
opts := binder.BindOptions{
    MaxBodySize:           64 << 10,
    DisallowUnknownFields: true,
}
```

```bash
# A key nothing binds is rejected
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d '{"name":"Carol","email":"carol@example.com","surprise":1}'
# 400 {"errors":{"surprise":"unknown field"}}
```

## Form Data Example

The server also accepts form-encoded data. Try this:

```bash
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "name=David&email=david@example.com&active=true"
```

Binder automatically detects the content type and parses accordingly.

## Advanced Features Shown

1. **Pointer Fields** - Using `*bool` to distinguish between `false` and "not provided"
2. **Slice Binding** - Arrays from JSON become Go slices
3. **Omitempty** - A body field marked `omitempty` keeps its current value when sent empty, as the update's `name` and `email` do
4. **Multiple Sources** - Combining path, query, body, and cookie data in one struct
5. **Content-Type Awareness** - Same handler works for JSON and form data
6. **Custom Validation** - Implementing the `Validator` interface with an error type of your own
7. **Text Types** - `uuid.UUID` binds from any source because it implements `encoding.TextUnmarshaler`; `time.Time` and `net.IP` bind the same way
8. **Embedded Structs** - `ListUsersRequest` embeds `Paging`; its fields are promoted as in `encoding/json`, and a bad `?page=` is reported as `page`

## Testing with Different Tools

### HTTPie
```bash
# Install: pip install httpie
http GET localhost:8080/users/1
http POST localhost:8080/users name=Eve email=eve@example.com active:=true tags:='["user"]'
```

### Postman
- Import the endpoints as a collection
- Set Content-Type to application/json for POST/PUT requests
- The middleware sets the api_key cookie on the first response, so list requests work from the second one on

## Error Scenarios

Try these to see error handling:

```bash
# Invalid user ID (non-integer)
curl http://localhost:8080/users/abc
# 400 {"errors":{"id":"invalid value"}}

# A team ID that is not a UUID
curl -b api_key=demo-key 'http://localhost:8080/users?team=platform'
# 400 {"errors":{"team":"invalid value"}}

# Missing required fields: both are reported
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d '{}'
# 400 {"errors":{"email":"required","name":"required"}}

# Several problems at once: a bad value and an unknown key
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d '{"name":"Carol","email":"carol@example.com","active":"maybe","surprise":1}'
# 400 {"errors":{"active":"invalid value","surprise":"unknown field"}}

# Binds, but fails validation: every broken rule is reported
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d '{"name":"Carol","email":"nope","tags":["a","b","c","d","e","f"]}'
# 422 {"errors":{"email":"is not a valid address","tags":"must have at most 5 entries"}}

# Invalid JSON: the request as a whole is rejected
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d 'invalid json'
# 400 {"error":"malformed request body: invalid JSON: ..."}
```

## Code Structure

- **Request Types** - Define what data to bind and from where
- **Validation** - Optional validation logic after binding
- **Handlers** - Standard HTTP handlers using binder for data extraction
- **Helpers** - JSON response utilities

This example shows how Binder simplifies REST API development while maintaining type safety and clear error handling.