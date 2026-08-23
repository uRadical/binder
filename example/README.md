# Binder Example - REST API Server

This example demonstrates how to use the Binder library to build a complete REST API server with all common binding scenarios.

## What This Example Shows

- **Path parameters** - `/users/{id}`
- **Query parameters** - `/users?active=true&limit=5`
- **Request bodies** - JSON data in POST/PUT requests
- **Cookies** - API key authentication
- **Headers** - Request tracing via `X-Request-ID`
- **Repeated values** - `/users?tags=admin&tags=user` filling a slice
- **Required fields** - Reporting a missing value rather than binding a zero one
- **Per-call options** - `BindWithOptions` for body limits and unknown fields
- **Validation** - Using the `Validator` interface
- **Partial updates** - Using `omitempty` for PATCH-like behavior
- **Error handling** - Choosing a status code from the error

## Running the Example

```bash
# From the example directory
cd example
go run main.go
```

The server will start on `http://localhost:8080`

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
# Demonstrates query parameters, repeated values and cookies
curl http://localhost:8080/users?active=true&limit=5
```

**Binder features:**
- `query:"active,omitempty"` - Optional boolean filter
- `query:"limit,omitempty"` - Optional integer limit
- `cookie:"api_key"` - API key from cookie (set automatically by middleware)

### 3. Create User
```bash
# Demonstrates JSON body binding and validation
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Charlie",
    "email": "charlie@example.com",
    "active": true,
    "tags": ["user", "premium"]
  }'
```

**Binder features:**
- `body:"name"` - Required field from JSON
- `body:"email"` - Required field from JSON
- `body:"active"` - Boolean from JSON
- `body:"tags"` - Slice of strings from JSON
- `Validate()` method - Custom validation after binding

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
- `body:"active,omitempty"` - Optional update to active status
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
    Active *bool    `body:"active,omitempty"`  // Pointer to distinguish false vs nil
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

// Implement binder.Validator interface
func (r CreateUserRequest) Validate() error {
    if !strings.Contains(r.Email, "@") {
        return fmt.Errorf("email %q is not a valid address", r.Email)
    }
    return nil
}
```

A missing required value reports which input was at fault:

```json
{
  "error": "missing required field Name: no body value named \"name\"",
  "field": "Name",
  "source": "body",
  "parameter": "name"
}
```

### Error Handling Pattern

Not every binding failure is the client's fault, so they do not all deserve a
400. The example routes each kind to the status that fits, and uses
`*binder.BindError` to say which input was at fault:

```go
switch {
case errors.Is(err, binder.ErrInvalidTarget):
    // A bug in this handler, not a bad request.
    respondError(w, "internal server error", http.StatusInternalServerError)

case errors.Is(err, binder.ErrBodyTooLarge):
    respondError(w, err.Error(), http.StatusRequestEntityTooLarge)

default:
    var bindErr *binder.BindError
    if errors.As(err, &bindErr) {
        // bindErr.Field, .Source and .Name identify the offending input
    }
    respondError(w, err.Error(), http.StatusBadRequest)
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
# {"error":"unknown field in request body: \"surprise\""}
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
3. **Omitempty** - Fields marked `omitempty` are optional
4. **Multiple Sources** - Combining path, query, body, and cookie data in one struct
5. **Content-Type Awareness** - Same handler works for JSON and form data
6. **Custom Validation** - Implementing the `Validator` interface

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
- The cookie will be set automatically by the middleware

## Error Scenarios

Try these to see error handling:

```bash
# Invalid user ID (non-integer)
curl http://localhost:8080/users/abc

# Missing required fields
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d '{}'

# Invalid JSON
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d 'invalid json'
```

## Code Structure

- **Request Types** - Define what data to bind and from where
- **Validation** - Optional validation logic after binding
- **Handlers** - Standard HTTP handlers using binder for data extraction
- **Helpers** - JSON response utilities

This example shows how Binder simplifies REST API development while maintaining type safety and clear error handling.