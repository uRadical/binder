// Package binder provides zero-dependency HTTP request binding for Go.
//
// Binder maps data from HTTP requests to Go structs using struct tags,
// supporting multiple data sources including path parameters, query strings,
// request bodies (JSON, form-encoded and multipart), headers and cookies.
//
// Basic usage:
//
//	var req struct {
//	    ID    int    `path:"id"`
//	    Name  string `query:"name"`
//	    Email string `body:"email"`
//	}
//	err := binder.Bind(r, &req)
//
// Path parameters are read with r.PathValue, as set by http.ServeMux patterns.
// Binder has no dependencies outside the standard library. Once binding
// succeeds, a target implementing Validator is validated before Bind returns;
// transformation is left to the caller.
package binder

import (
	"bytes"
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Tag constants
const (
	path   = "path"
	query  = "query"
	body   = "body"
	jjson  = "json"
	cookie = "cookie"
	header = "header"
)

// Tag option constants
const (
	optOmitEmpty = "omitempty"
	optRequired  = "required"
)

// bindSources lists the tag sources in precedence order. The first source
// present on a field is the one it binds from. Header is last so that adding
// it did not change which tag an existing field binds from.
var bindSources = [...]string{path, query, body, jjson, cookie, header}

// fieldTag returns the active binding tag for a struct field: the source it
// binds from, the name to look up in that source, and the options that follow
// the name. ok is false when the field carries no binding tag.
func fieldTag(field reflect.StructField) (source, name, opts string, ok bool) {
	for _, src := range bindSources {
		if tag := field.Tag.Get(src); tag != "" {
			name, opts = splitTag(tag)
			return src, name, opts, true
		}
	}
	return "", "", "", false
}

// hasOption reports whether a comma-separated tag option list contains opt.
// It compares whole options, so a name such as "omit" combined with a
// neighbouring "empty" is never mistaken for "omitempty".
func hasOption(opts, opt string) bool {
	for opts != "" {
		var cur string
		cur, opts = splitTag(opts)
		if cur == opt {
			return true
		}
	}
	return false
}

// splitTag separates a struct tag value into the source name and its
// comma-separated options. Tags are written as `source:"name,opt,..."`, so the
// name is everything before the first comma and the options are what follows:
//
//	`body:"email,omitempty"` -> "email", "omitempty"
//	`body:"email"`           -> "email", ""
func splitTag(tag string) (name, opts string) {
	if i := strings.Index(tag, ","); i != -1 {
		return tag[:i], tag[i+1:]
	}
	return tag, ""
}

// fieldInfo is one struct field's binding tag, resolved once per type. Holding
// the parsed name and options here keeps tag parsing off the per-request path.
type fieldInfo struct {
	Index     int                 // position of the field within the struct
	FieldType reflect.StructField // retained for error reporting
	Source    string              // "path", "query", "body", "json", "cookie"
	TagName   string              // key to look up in Source, without options
	OmitEmpty bool
	Required  bool
	IsSlice   bool     // destination is a slice, so repeated values all bind
	Fast      fastKind // set straight from a JSON token, skipping conversion
}

// fastKind names the destinations a JSON token can fill without going through
// an interface value. Anything else, including a type with its own
// TextUnmarshaler, takes the general path.
type fastKind uint8

const (
	fastNone fastKind = iota
	fastString
	fastInt
	fastUint
	fastFloat
	fastBool
)

// fastKindOf reports how a field can be filled from a JSON token. Only
// predeclared types qualify: a named type may define UnmarshalText, and a
// conversion must be given the chance to run.
func fastKindOf(t reflect.Type) fastKind {
	if t.PkgPath() != "" {
		return fastNone // a named type, which may unmarshal itself
	}
	if t.Implements(textUnmarshalerType) || reflect.PointerTo(t).Implements(textUnmarshalerType) {
		return fastNone
	}

	switch t.Kind() {
	case reflect.String:
		return fastString
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return fastInt
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return fastUint
	case reflect.Float32, reflect.Float64:
		return fastFloat
	case reflect.Bool:
		return fastBool
	default:
		return fastNone
	}
}

// textUnmarshalerType is resolved once: tryTextUnmarshaler consults it for
// every field of every request.
var textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()

// typeInfo is everything binding needs to know about a struct type, resolved
// once. The body key set lives here rather than being rebuilt per request,
// which would allocate on every call including those with no body at all.
type typeInfo struct {
	fields   []fieldInfo
	bodyKeys map[string]struct{}
	// bodyFields indexes fields by the body member they bind, so a token walk
	// can find the destination without a second pass.
	bodyFields map[string]int
}

// Cache of resolved type information. It is keyed by type and so is bounded by
// the number of struct types a program binds into.
var fieldCache = make(map[reflect.Type]*typeInfo)
var fieldCacheMutex sync.RWMutex

// Validator is an optional interface that structs can implement to provide
// custom validation logic that runs automatically after successful binding.
// Bind passes the request's context, so rules can use its deadline and
// cancellation or request-scoped values such as the authenticated user.
// Rules that need neither simply ignore it.
//
// Example:
//
//	type CreateUserRequest struct {
//	    Email string `body:"email"`
//	    Age   int    `body:"age"`
//	}
//
//	func (r CreateUserRequest) Validate(ctx context.Context) error {
//	    if r.Age < 18 {
//	        return errors.New("user must be 18 or older")
//	    }
//	    return nil
//	}
//
// When a type implements Validator, Bind will call Validate after binding
// and return any validation errors, wrapped so that errors.Is and errors.As
// still reach them; a cancelled request matches context.Canceled.
type Validator interface {
	Validate(ctx context.Context) error
}

// DefaultMaxBodySize is the largest request body, in bytes, that Bind reads,
// and the limit BindWithOptions applies when BindOptions.MaxBodySize is zero.
// A larger body is rejected with ErrBodyTooLarge rather than buffered, so that
// a single request cannot exhaust server memory.
const DefaultMaxBodySize int64 = 10 << 20 // 10 MB

// ErrBodyTooLarge is returned when a request body exceeds the body size limit.
// Handlers should treat it as http.StatusRequestEntityTooLarge:
//
//	if errors.Is(err, binder.ErrBodyTooLarge) {
//	    http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
//	    return
//	}
var ErrBodyTooLarge = errors.New("request body too large")

// ErrMalformedBody is returned by Bind when the request body cannot be parsed
// as the format its Content-Type declares. Handlers should treat it as
// http.StatusBadRequest:
//
//	if errors.Is(err, binder.ErrMalformedBody) {
//	    http.Error(w, "malformed request body", http.StatusBadRequest)
//	    return
//	}
//
// A body whose Content-Type is none of JSON, form-encoded or multipart is not
// parsed at all and so is never malformed; such a request binds from its path,
// query, header and cookie values alone.
var ErrMalformedBody = errors.New("malformed request body")

// ErrInvalidTarget is returned by Bind when the destination is not a non-nil
// pointer to a struct. Unlike ErrBodyTooLarge and ErrMalformedBody it reports
// a programming error rather than a bad request, so a handler that sees it
// should answer http.StatusInternalServerError rather than blaming the client.
var ErrInvalidTarget = errors.New("invalid bind target")

// ErrMissingRequired is returned by Bind when a field tagged with the
// "required" option had no value in its source. It is wrapped by a BindError
// naming the field, so errors.As gives the detail and errors.Is gives the
// category.
var ErrMissingRequired = errors.New("missing required value")

// ErrUnknownField is returned by BindWithOptions when DisallowUnknownFields is
// set and the request body carries a key that no field of the target binds.
var ErrUnknownField = errors.New("unknown field in request body")

// BindError describes one failure in binding the target struct: a field that
// could not be bound, or a body member no field binds. It carries the input at
// fault alongside the field, so a handler can say which part of the request to
// fix rather than only that something failed.
//
// Key client-facing output on Name, the key the client sent, which is always
// set. Field is the Go-side name, for logs and debugging, and is empty for an
// unknown body member, since no field binds it.
//
//	var errs binder.BindErrors
//	if errors.As(err, &errs) {
//	    for _, e := range errs {
//	        fmt.Printf("%s %q: %v\n", e.Source, e.Name, e.Err)
//	    }
//	}
type BindError struct {
	// Field is the Go struct field, as a path for a nested struct or slice
	// element, such as Address.Postcode or Tags[2]. It is empty for a body
	// member that no field binds, reported with DisallowUnknownFields.
	Field string

	// Source is the tag source the value was read from, such as "query".
	Source string

	// Name is the key the client sent in that source, as a path for a nested
	// value, such as address.postcode or tags[2]. It is always set.
	Name string

	// Message is a complete description for logs. Its wording is not part of
	// the compatibility promise and may change in any release.
	Message string

	// Err is the underlying cause, reachable with errors.Is and errors.As:
	// ErrMissingRequired for a missing required value, ErrUnknownField for an
	// unknown body member, and otherwise the conversion error.
	Err error
}

func (e *BindError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "bind error"
}

// Unwrap exposes the underlying cause to errors.Is and errors.As.
func (e *BindError) Unwrap() error { return e.Err }

// BindOptions configures a single call to BindWithOptions. The zero value
// behaves exactly as Bind does.
type BindOptions struct {
	// MaxBodySize is the largest request body, in bytes, this call reads.
	// Zero applies DefaultMaxBodySize, and a negative value removes the limit.
	MaxBodySize int64

	// DisallowUnknownFields makes binding fail with ErrUnknownField when the
	// request body carries a top-level key that no field of the target binds.
	// Keys nested inside objects are not inspected.
	DisallowUnknownFields bool
}

// maxBodySize resolves the body limit for a call. Zero means the default
// rather than no limit, so that setting only another option cannot silently
// remove the cap.
func (o BindOptions) maxBodySize() int64 {
	if o.MaxBodySize == 0 {
		return DefaultMaxBodySize
	}
	return o.MaxBodySize
}

// Bind maps data from an HTTP request into a struct using reflection and struct tags.
//
// The target must be a pointer to a struct. Bind supports multiple data sources:
//
//   - path:"name"   - URL path parameters, read with r.PathValue
//   - query:"name"  - URL query parameters
//   - body:"name"   - Request body (JSON, form-encoded or multipart, by Content-Type)
//   - json:"name"   - Alternative to body tag for JSON data
//   - cookie:"name" - HTTP cookies
//   - header:"name" - HTTP request headers, matched case-insensitively
//
// Tag modifiers:
//
//   - omitempty - Skip binding if the value is present but empty
//   - required  - Return an error if the value is missing from its source
//
// Example:
//
//	type UpdateUserRequest struct {
//	    ID       int    `path:"id"`
//	    Name     string `body:"name"`
//	    Email    string `body:"email,omitempty"`
//	    APIToken string `cookie:"api_token"`
//	    TraceID  string `header:"X-Request-ID"`
//	}
//
//	var req UpdateUserRequest
//	if err := binder.Bind(r, &req); err != nil {
//	    // Handle binding error
//	}
//
// Fields that reflection cannot set, meaning unexported ones, are ignored even
// when they carry a binding tag.
//
// Returns an error if:
//   - The target is not a non-nil pointer to a struct (see ErrInvalidTarget)
//   - Type conversion fails
//   - Required fields are missing
//   - The request body exceeds DefaultMaxBodySize (see ErrBodyTooLarge)
//   - The request body cannot be parsed (see ErrMalformedBody)
//   - Validation fails (if the struct implements Validator)
func Bind(r *http.Request, i any) error {
	return BindWithOptions(r, i, BindOptions{MaxBodySize: DefaultMaxBodySize})
}

// BindWithOptions is Bind with per-call configuration. The zero BindOptions
// behaves exactly as Bind does.
//
// Example:
//
//	opts := binder.BindOptions{
//	    MaxBodySize:           1 << 20,
//	    DisallowUnknownFields: true,
//	}
//	if err := binder.BindWithOptions(r, &req, opts); err != nil {
//	    // Handle binding error
//	}
func BindWithOptions(r *http.Request, i any, opts BindOptions) error {
	if r == nil {
		return fmt.Errorf("%w: cannot bind from a nil request", ErrInvalidTarget)
	}

	typ, val, err := targetStruct(i)
	if err != nil {
		return err
	}

	// Resolve the type once: binding, the body key set and the unknown-field
	// check all draw on it, and each lookup takes the cache lock.
	info := typeInfoFor(typ)

	// Decoding only the body members some field binds is faster, but the
	// unknown-field check needs to see the ones nothing binds, so that option
	// decodes everything.
	var wanted map[string]struct{}
	if !opts.DisallowUnknownFields {
		wanted = info.bodyKeys
	}

	// A field that fails is recorded and binding moves on, so a client sees
	// every bad input in one response. A failure of the request as a whole,
	// such as an unreadable body, still ends binding at once, since nothing
	// bound after it could be trusted.
	var errs fieldErrs

	// Parse request body once
	bodyData, bound, unknown, err := parseRequestBody(r, opts.maxBodySize(), wanted, info, val, opts.DisallowUnknownFields, &errs)
	if err != nil {
		return err
	}

	// Process each field in the struct
	bindStructFields(r, info, val, bodyData, bound, &errs)

	if opts.DisallowUnknownFields {
		if bodyData != nil {
			unknown = unknownKeys(info, bodyData)
		}
		errs.unknown = unknownFieldErrors(unknown)
	}

	// Validate runs only on a fully bound target: a field that failed is
	// left at its zero value, and rules checking it would report a second,
	// misleading failure for the same input.
	if err := errs.list(); err != nil {
		return err
	}
	return validate(r.Context(), i)
}

// BindErrors is the error Bind returns when any field fails to bind, whether
// one or several: every field is attempted, so a client can correct all of its
// input in one round trip. Entries are in the order the struct declares its
// fields, followed by any unknown body members.
//
//	var errs binder.BindErrors
//	if errors.As(err, &errs) {
//	    for _, e := range errs {
//	        problems[e.Name] = e.Message
//	    }
//	}
//
// A failure within a nested struct or a slice is its own entry, its Field and
// Name giving the path to it, such as Address.Postcode and address.postcode,
// or Tags[2] and tags[2].
//
// Failures of the request as a whole, such as ErrMalformedBody, are not
// BindErrors: they end binding at once, since nothing bound after them could
// be trusted.
type BindErrors []*BindError

// Error lists the failures one per line.
func (e BindErrors) Error() string {
	msgs := make([]string, len(e))
	for i, err := range e {
		msgs[i] = err.Error()
	}
	return strings.Join(msgs, "\n")
}

// Unwrap exposes each failure to errors.Is and errors.As, so that
// errors.Is(err, ErrMissingRequired) reports whether any field was missing.
func (e BindErrors) Unwrap() []error {
	errs := make([]error, len(e))
	for i, err := range e {
		errs[i] = err
	}
	return errs
}

// fieldErrs gathers binding failures by field index, so they are reported in
// field order whichever source found them. It stays nil until a field fails,
// so a successful bind allocates nothing for it.
type fieldErrs struct {
	byField [][]*BindError
	unknown []*BindError
}

func (e *fieldErrs) set(info *typeInfo, index int, errs []*BindError) {
	if e.byField == nil {
		e.byField = make([][]*BindError, len(info.fields))
	}
	e.byField[index] = errs
}

// list returns the gathered failures as BindErrors, or nil when there are
// none. The result is typed error so that no failures gives a true nil.
func (e *fieldErrs) list() error {
	var all BindErrors
	for _, errs := range e.byField {
		all = append(all, errs...)
	}
	all = append(all, e.unknown...)
	if len(all) == 0 {
		return nil
	}
	return all
}

// validate runs the target's Validator, if it has one.
func validate(ctx context.Context, i any) error {
	if v, ok := i.(Validator); ok {
		if err := v.Validate(ctx); err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}
	}
	return nil
}

// targetStruct validates the destination given to Bind and returns the struct
// type and value to bind into. Bind's contract is a non-nil pointer to a
// struct, and anything else is reported as ErrInvalidTarget rather than left
// to panic inside the reflect package.
func targetStruct(i any) (reflect.Type, reflect.Value, error) {
	if i == nil {
		return nil, reflect.Value{}, fmt.Errorf("%w: target is nil", ErrInvalidTarget)
	}

	val := reflect.ValueOf(i)
	if val.Kind() != reflect.Ptr {
		return nil, reflect.Value{}, fmt.Errorf("%w: target is %s, want a pointer to a struct", ErrInvalidTarget, val.Type())
	}
	if val.IsNil() {
		return nil, reflect.Value{}, fmt.Errorf("%w: target is a nil %s", ErrInvalidTarget, val.Type())
	}

	elem := val.Elem()
	if elem.Kind() != reflect.Struct {
		return nil, reflect.Value{}, fmt.Errorf("%w: target is %s, want a pointer to a struct", ErrInvalidTarget, val.Type())
	}

	return elem.Type(), elem, nil
}

// parseRequestBody reads and parses the request body, restoring it for other
// readers.
//
// A JSON body is bound straight into the target where the build allows it,
// which returns a nil map, the set of fields it filled and, when wantUnknown is
// set, the members nothing binds; a field it could not fill is recorded in
// errs. Every other format returns the map that binding reads from, and a nil
// set.
func parseRequestBody(r *http.Request, maxBodySize int64, wanted map[string]struct{}, info *typeInfo, val reflect.Value, wantUnknown bool, errs *fieldErrs) (map[string]any, []bool, []string, error) {
	// Content-Length is not consulted here: a chunked request declares no
	// length at all, so skipping on a non-positive Content-Length would drop
	// its body entirely. Whether a body is empty is decided after reading.
	if r.Body == nil || r.Body == http.NoBody {
		return make(map[string]any), nil, nil, nil
	}

	// Read the body once, refusing anything oversized
	bodyBytes, err := readBody(r, maxBodySize)
	if err != nil {
		return nil, nil, nil, err
	}

	// Restore the body for other potential readers
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	// An absent body is not a malformed one, so an empty read is reported as
	// no data rather than handed to a parser that would reject it.
	if len(bodyBytes) == 0 {
		return make(map[string]any), nil, nil, nil
	}

	// A JSON body is handled before the other formats: where the build allows
	// it, members go straight into their fields rather than through a map.
	if isJSONContentType(parseContentType(r.Header.Get("Content-Type"))) {
		// A failure to convert one member concerns that field and is
		// recorded in errs; only a failure to read the body is returned.
		bodyData, bound, unknown, err := jsonBodyInto(bodyBytes, info, val, wanted, wantUnknown, errs)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%w: invalid JSON: %w", ErrMalformedBody, err)
		}
		return bodyData, bound, unknown, nil
	}

	// Create a copy of the request with the new body for parsing
	rCopy := *r
	rCopy.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	// Parse the body. A body that cannot be parsed is reported rather than
	// discarded: binding would otherwise report success while every
	// body-sourced field was silently left at its zero value.
	bodyData, err := parseBody(rCopy, bodyBytes, wanted)
	return bodyData, nil, nil, err
}

// readBody reads the whole request body, refusing bodies larger than
// limit. The limit is enforced while reading rather than trusting
// Content-Length, which the client controls and may understate. An oversized
// body is reported as an error rather than truncated, so that a request is
// never bound from a partial body.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	if limit <= 0 {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("error reading request body: %w", err)
		}
		return bodyBytes, nil
	}

	// Reject an honestly declared oversized body without reading it at all.
	if r.ContentLength > limit {
		return nil, fmt.Errorf("%w: %d bytes declared, limit is %d", ErrBodyTooLarge, r.ContentLength, limit)
	}

	// Read one byte past the limit so an understated Content-Length is
	// detected instead of silently truncating the body.
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("error reading request body: %w", err)
	}
	if int64(len(bodyBytes)) > limit {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrBodyTooLarge, limit)
	}

	return bodyBytes, nil
}

// unknownKeys returns the body keys that no field of the target binds. Only
// top-level keys are considered, since nested values are bound by the nested
// struct rather than by a tag on this one.
func unknownKeys(info *typeInfo, bodyData map[string]any) []string {
	var unknown []string
	for name := range bodyData {
		if _, found := info.bodyKeys[name]; !found {
			unknown = append(unknown, name)
		}
	}
	return unknown
}

// unknownFieldErrors reports each body key that no field of the target binds,
// sorted so the result does not vary with map iteration or member order.
func unknownFieldErrors(unknown []string) []*BindError {
	slices.Sort(unknown)
	errs := make([]*BindError, len(unknown))
	for i, name := range unknown {
		errs[i] = &BindError{
			Source:  body,
			Name:    name,
			Message: fmt.Sprintf("unknown field %q in request body", name),
			Err:     ErrUnknownField,
		}
	}
	return errs
}

// queryCache parses a request's query string on first use and reuses it for
// the rest of the call. net/url reparses on every call to URL.Query, so a
// struct with several query tags would otherwise parse the whole string once
// per field. Parsing is deferred so that a target binding from no query
// parameter at all does not pay for it.
type queryCache struct {
	url    *url.URL
	parsed url.Values
}

func (q *queryCache) ensure() url.Values {
	if q.parsed == nil {
		q.parsed = q.url.Query()
	}
	return q.parsed
}

func (q *queryCache) get(name string) string { return q.ensure().Get(name) }

// all returns every value given for a parameter, for binding into a slice.
func (q *queryCache) all(name string) []string { return q.ensure()[name] }

// bindStructFields processes each bindable field in the struct and binds data
// from the request, recording in errs each field that fails.
func bindStructFields(r *http.Request, info *typeInfo, val reflect.Value, bodyData map[string]any, bound []bool, errs *fieldErrs) {
	queries := queryCache{url: r.URL}

	for index, fi := range info.fields {
		// A body field the JSON walk already filled needs nothing further.
		if bound != nil && bound[index] {
			continue
		}

		// Extract value from appropriate source
		value, exists, err := extractFieldValue(r, fi, bodyData, &queries)
		if err != nil {
			errs.set(info, index, fieldFailures(fi, err))
			continue
		}

		// A missing value is an error when the field is tagged required,
		// and is otherwise simply left at its zero value.
		if !exists {
			if fi.Required {
				errs.set(info, index, []*BindError{missingRequiredError(fi)})
			}
			continue
		}

		// Skip if the value should be omitted
		if fi.OmitEmpty && isEmptyValue(value) {
			continue
		}

		// Set the field value
		if err := bindFieldValue(val.Field(fi.Index), value); err != nil {
			errs.set(info, index, fieldFailures(fi, err))
		}
	}
}

// extractFieldValue gets the value for a field from the appropriate request source
func extractFieldValue(r *http.Request, fi fieldInfo, bodyData map[string]any, queries *queryCache) (any, bool, error) {
	switch fi.Source {
	case path:
		v := r.PathValue(fi.TagName)
		return v, v != "", nil

	case query:
		if fi.IsSlice {
			vs := queries.all(fi.TagName)
			return vs, len(vs) > 0, nil
		}
		v := queries.get(fi.TagName)
		return v, v != "", nil

	case body, jjson:
		v, exists := bodyData[fi.TagName]
		return v, exists, nil

	case cookie:
		c, err := r.Cookie(fi.TagName)
		if err == nil {
			return c.Value, true, nil
		}
		return nil, false, nil

	case header:
		// Header.Get and Header.Values canonicalise the name, so
		// `header:"x-request-id"` and `header:"X-Request-ID"` name the same
		// header.
		if fi.IsSlice {
			vs := r.Header.Values(fi.TagName)
			return vs, len(vs) > 0, nil
		}
		v := r.Header.Get(fi.TagName)
		return v, v != "", nil

	default:
		return nil, false, nil
	}
}

// missingRequiredError reports a field tagged with the "required" option that
// had no value in its source. For path and query parameters an empty value
// counts as missing, since neither source distinguishes the two.
func missingRequiredError(fi fieldInfo) *BindError {
	return &BindError{
		Field:   fi.FieldType.Name,
		Source:  fi.Source,
		Name:    fi.TagName,
		Message: fmt.Sprintf("missing required field %s: no %s value named %q", fi.FieldType.Name, fi.Source, fi.TagName),
		Err:     ErrMissingRequired,
	}
}

// bindFieldValue sets the value on a struct field, handling nested structs and
// pointers.
func bindFieldValue(fieldVal reflect.Value, value any) error {
	if fieldVal.Kind() == reflect.Ptr && fieldVal.IsNil() {
		fieldVal.Set(reflect.New(fieldVal.Type().Elem())) // Initialize pointer fields
	}
	return setField(fieldVal, value)
}

// fieldFailures turns a failure to set a field into BindErrors carrying the
// field's binding source, so that a caller can report which input was at
// fault. A nested struct or slice reports BindErrors of its own, relative to
// the field; those are placed beneath it.
func fieldFailures(fi fieldInfo, err error) []*BindError {
	var inner BindErrors
	if errors.As(err, &inner) {
		return nestFailures(fi.FieldType.Name, fi.TagName, fi.Source, inner)
	}
	return []*BindError{newBindError(fi.FieldType.Name, fi.Source, fi.TagName, err)}
}

// nestFailures places failures reported by a nested struct or slice beneath
// the field holding it, joining their paths to the field's.
func nestFailures(field, name, source string, inner BindErrors) []*BindError {
	errs := make([]*BindError, len(inner))
	for i, e := range inner {
		errs[i] = newBindError(joinPath(field, e.Field), source, joinPath(name, e.Name), e.Err)
	}
	return errs
}

// joinPath appends a nested path to its parent: a field as parent.child, a
// slice element as parent[i].
func joinPath(parent, child string) string {
	if strings.HasPrefix(child, "[") {
		return parent + child
	}
	return parent + "." + child
}

// newBindError builds the BindError for a value that could not be set.
func newBindError(field, source, name string, err error) *BindError {
	return &BindError{
		Field:   field,
		Source:  source,
		Name:    name,
		Message: fmt.Sprintf("error setting field %s: %v", field, err),
		Err:     err,
	}
}

// getFieldInfo returns the binding tags of a struct type, resolving them on
// first use and reusing them afterwards. Only settable, tagged fields appear,
// so callers need not re-check either condition.
func getFieldInfo(typ reflect.Type) []fieldInfo { return typeInfoFor(typ).fields }

// typeInfoFor resolves a struct type's binding tags on first use and reuses
// them afterwards.
func typeInfoFor(typ reflect.Type) *typeInfo {
	fieldCacheMutex.RLock()
	cached, found := fieldCache[typ]
	fieldCacheMutex.RUnlock()

	if found {
		return cached
	}

	fieldCacheMutex.Lock()
	defer fieldCacheMutex.Unlock()

	// Check again in case another goroutine built it while we were waiting
	if cached, found = fieldCache[typ]; found {
		return cached
	}

	info := make([]fieldInfo, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)

		// Unexported fields cannot be set through reflection, so they are
		// ignored even when tagged, as encoding/json does.
		if !field.IsExported() {
			continue
		}

		source, name, opts, ok := fieldTag(field)
		if !ok {
			continue
		}

		// A pointer to a slice takes many values just as a slice does.
		fieldType := field.Type
		if fieldType.Kind() == reflect.Ptr {
			fieldType = fieldType.Elem()
		}

		info = append(info, fieldInfo{
			Index:     i,
			FieldType: field,
			Source:    source,
			TagName:   name,
			OmitEmpty: hasOption(opts, optOmitEmpty),
			Required:  hasOption(opts, optRequired),
			IsSlice:   fieldType.Kind() == reflect.Slice,
			Fast:      fastKindOf(field.Type),
		})
	}

	keys := make(map[string]struct{})
	fields := make(map[string]int)
	for i, fi := range info {
		if fi.Source == body || fi.Source == jjson {
			keys[fi.TagName] = struct{}{}
			fields[fi.TagName] = i
		}
	}

	cached = &typeInfo{fields: info, bodyKeys: keys, bodyFields: fields}
	fieldCache[typ] = cached
	return cached
}

// bindNestedFields binds map data into a struct's fields, matching each on its
// body tag or the json alias. It is the struct case of setField.
//
// Every field is attempted, and the failures are returned as BindErrors whose
// paths are relative to target.
func bindNestedFields(target reflect.Value, data map[string]any) error {
	var errs BindErrors
	typ := target.Type()
	for i := 0; i < typ.NumField(); i++ {
		fieldType := typ.Field(i)

		// Unexported fields cannot be set through reflection, so they are
		// ignored even when tagged, as the top-level fields are.
		if !fieldType.IsExported() {
			continue
		}

		tag := fieldType.Tag.Get(body)
		if tag == "" {
			tag = fieldType.Tag.Get(jjson)
		}
		if tag == "" {
			continue
		}
		name, _ := splitTag(tag)

		nestedValue, ok := data[name]
		if !ok {
			continue
		}

		if err := setField(target.Field(i), nestedValue); err != nil {
			var inner BindErrors
			if errors.As(err, &inner) {
				errs = append(errs, nestFailures(fieldType.Name, name, body, inner)...)
			} else {
				errs = append(errs, newBindError(fieldType.Name, body, name, err))
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// parseContentType extracts the content type from the Content-Type header
func parseContentType(header string) string {
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, "=") {
			return strings.ToLower(part)
		}
	}
	return ""
}

// isJSONContentType reports whether a media type carries JSON. Besides
// application/json it accepts the structured syntax suffix of RFC 6839, so
// application/vnd.api+json, application/hal+json and application/problem+json
// are recognised, as is the non-standard but common text/json. A body left
// unrecognised is not parsed at all, and so binds nothing without saying so.
func isJSONContentType(ct string) bool {
	_, subtype, found := strings.Cut(ct, "/")
	if !found {
		return false
	}
	return subtype == jjson || strings.HasSuffix(subtype, "+"+jjson)
}

// parseBody extracts and parses the request body into a map
func parseBody(r http.Request, bodyBytes []byte, wanted map[string]struct{}) (map[string]any, error) {
	var reqBody map[string]any
	ct := parseContentType(r.Header.Get("Content-Type"))

	switch {
	case ct == "multipart/form-data":
		reqBody, err := parseMultipartBody(r.Header.Get("Content-Type"), bodyBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid multipart form: %w", ErrMalformedBody, err)
		}
		return reqBody, nil

	case ct == "application/x-www-form-urlencoded":
		reqBody = make(map[string]any)
		err := r.ParseForm()
		if err != nil {
			return nil, fmt.Errorf("%w: invalid form data: %w", ErrMalformedBody, err)
		}
		for k, v := range r.PostForm {
			if len(v) == 1 {
				reqBody[k] = v[0]
			} else {
				reqBody[k] = v
			}
		}
		return reqBody, nil
	}

	return make(map[string]any), nil
}

// fileHeaderType and fileHeaderSliceType are the destinations an uploaded file
// binds to.
var (
	fileHeaderType      = reflect.TypeOf((*multipart.FileHeader)(nil))
	fileHeaderSliceType = reflect.TypeOf([]*multipart.FileHeader(nil))
)

// parseMultipartBody reads a multipart form into the same shape the other body
// formats produce: text parts as strings, and file parts as *FileHeader.
//
// The whole body has already been read and bounded by the size limit, so the
// parser is given that same allowance and never spills a part to a temporary
// file. Raise BindOptions.MaxBodySize on an endpoint that accepts uploads.
func parseMultipartBody(contentType string, bodyBytes []byte) (map[string]any, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, err
	}
	boundary, ok := params["boundary"]
	if !ok {
		return nil, errors.New("no boundary in Content-Type")
	}

	// The body is already in memory and within the limit, so allow the parser
	// to keep all of it there rather than writing parts to disk.
	maxMemory := int64(len(bodyBytes)) + 1

	form, err := multipart.NewReader(bytes.NewReader(bodyBytes), boundary).ReadForm(maxMemory)
	if err != nil {
		return nil, err
	}

	reqBody := make(map[string]any, len(form.Value)+len(form.File))
	for name, values := range form.Value {
		if len(values) == 1 {
			reqBody[name] = values[0]
		} else {
			reqBody[name] = values
		}
	}
	for name, files := range form.File {
		if len(files) == 1 {
			reqBody[name] = files[0]
		} else {
			reqBody[name] = files
		}
	}
	return reqBody, nil
}

// setFileHeader binds an uploaded file, or a set of them, to a field declared
// as *multipart.FileHeader or []*multipart.FileHeader. Reports whether the
// value was a file part at all.
func setFileHeader(field reflect.Value, value any) (bool, error) {
	single, isSingle := value.(*multipart.FileHeader)
	many, isMany := value.([]*multipart.FileHeader)
	if !isSingle && !isMany {
		return false, nil
	}

	switch field.Type() {
	case fileHeaderType:
		if isMany {
			// More than one part was sent for a field that takes one file.
			if len(many) == 0 {
				return true, errors.New("no file in upload")
			}
			single = many[0]
		}
		field.Set(reflect.ValueOf(single))
		return true, nil

	case fileHeaderSliceType:
		if isSingle {
			many = []*multipart.FileHeader{single}
		}
		field.Set(reflect.ValueOf(many))
		return true, nil

	default:
		return true, fmt.Errorf("cannot bind an uploaded file to %s", field.Type())
	}
}

// setField sets the appropriate value on the given reflect.Value field
func setField(field reflect.Value, value any) error {
	// Handle nil value
	if value == nil {
		return nil
	}

	// An uploaded file is not converted, it is handed over as it arrived.
	if handled, err := setFileHeader(field, value); handled {
		return err
	}

	// Handle TextUnmarshaler interface
	handled, err := tryTextUnmarshaler(field, value)
	if handled {
		return err
	}

	// Handle based on field kind
	return setFieldByKind(field, value)
}

// tryTextUnmarshaler attempts to use TextUnmarshaler interface if implemented
// Returns (handled, error) where handled indicates if TextUnmarshaler was used
func tryTextUnmarshaler(field reflect.Value, value any) (bool, error) {
	if field.Type().Implements(textUnmarshalerType) {
		// A nil pointer has nothing to unmarshal into, and UnmarshalText
		// would dereference it. Give it a value first, as the kind-based
		// paths further down do for pointers they handle themselves.
		if field.Kind() == reflect.Ptr && field.IsNil() {
			if !field.CanSet() {
				return false, nil
			}
			field.Set(reflect.New(field.Type().Elem()))
		}

		strVal, ok := unmarshalText(value)
		if !ok {
			return true, errors.New("value is not a string for TextUnmarshaler")
		}
		return true, field.Interface().(encoding.TextUnmarshaler).UnmarshalText(strVal)
	}

	if field.CanAddr() && reflect.PointerTo(field.Type()).Implements(textUnmarshalerType) {
		strVal, ok := unmarshalText(value)
		if !ok {
			return true, errors.New("value is not a string for TextUnmarshaler")
		}
		return true, field.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText(strVal)
	}

	return false, nil // No TextUnmarshaler interface found
}

// unmarshalText returns the bytes to hand a TextUnmarshaler, for the value
// kinds a request body can produce.
func unmarshalText(value any) ([]byte, bool) {
	switch v := value.(type) {
	case string:
		return []byte(v), true
	case []byte:
		return v, true
	default:
		return nil, false
	}
}

// setFieldByKind sets the field value based on its reflect.Kind
func setFieldByKind(field reflect.Value, value any) error {
	switch field.Kind() {
	case reflect.String:
		return setString(field, value)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return setInt(field, value)

	case reflect.Float32, reflect.Float64:
		return setFloat(field, value)

	case reflect.Bool:
		return setBool(field, value)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return setUint(field, value)

	case reflect.Slice:
		return setSlice(field, value)

	case reflect.Array:
		return fmt.Errorf("arrays are not supported, use slices instead")

	case reflect.Struct:
		return setStruct(field, value)

	case reflect.Ptr:
		if field.IsNil() {
			field.Set(reflect.New(field.Type().Elem()))
		}
		return setField(field.Elem(), value)

	default:
		return fmt.Errorf("unsupported type: %s", field.Kind())
	}
}

// setString sets a string value to a field
func setString(field reflect.Value, value any) error {
	str, err := toString(value)
	if err != nil {
		return err
	}
	field.SetString(str)
	return nil
}

// toString converts various types to string
func toString(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	case fmt.Stringer:
		return v.String(), nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// setIntChecked writes an integer, refusing one the field cannot hold.
// reflect truncates silently, so 9999 into an int8 would otherwise bind as 15.
func setIntChecked(field reflect.Value, n int64) error {
	if field.OverflowInt(n) {
		return fmt.Errorf("%d overflows %s", n, field.Type())
	}
	field.SetInt(n)
	return nil
}

// setUintChecked writes an unsigned integer, refusing one the field cannot
// hold.
func setUintChecked(field reflect.Value, n uint64) error {
	if field.OverflowUint(n) {
		return fmt.Errorf("%d overflows %s", n, field.Type())
	}
	field.SetUint(n)
	return nil
}

// setFloatChecked writes a float, refusing one the field cannot hold.
func setFloatChecked(field reflect.Value, n float64) error {
	if field.OverflowFloat(n) {
		return fmt.Errorf("%v overflows %s", n, field.Type())
	}
	field.SetFloat(n)
	return nil
}

// setInt sets an integer value to a field
func setInt(field reflect.Value, value any) error {
	switch v := value.(type) {
	case int:
		return setIntChecked(field, int64(v))
	case int8:
		return setIntChecked(field, int64(v))
	case int16:
		return setIntChecked(field, int64(v))
	case int32:
		return setIntChecked(field, int64(v))
	case int64:
		return setIntChecked(field, v)
	case float32:
		return setIntChecked(field, int64(v))
	case float64:
		return setIntChecked(field, int64(v))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return setIntChecked(field, i)
		}
		// Numbers written in a form Int64 rejects, such as 1e5 or 1.0, went
		// through float64 before and still do.
		f, err := v.Float64()
		if err != nil {
			return err
		}
		return setIntChecked(field, int64(f))
	case string:
		i, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return err
		}
		return setIntChecked(field, i)
	default:
		return fmt.Errorf("cannot convert %T to int", value)
	}
}

// setUint sets an unsigned integer value to a field
func setUint(field reflect.Value, value any) error {
	switch v := value.(type) {
	case uint:
		return setUintChecked(field, uint64(v))
	case uint8:
		return setUintChecked(field, uint64(v))
	case uint16:
		return setUintChecked(field, uint64(v))
	case uint32:
		return setUintChecked(field, uint64(v))
	case uint64:
		return setUintChecked(field, v)
	case int:
		if v < 0 {
			return fmt.Errorf("cannot convert negative int to uint")
		}
		return setUintChecked(field, uint64(v))
	case float64:
		if v < 0 {
			return fmt.Errorf("cannot convert negative float to uint")
		}
		return setUintChecked(field, uint64(v))
	case json.Number:
		if u, err := strconv.ParseUint(v.String(), 10, 64); err == nil {
			return setUintChecked(field, u)
		}
		f, err := v.Float64()
		if err != nil {
			return err
		}
		if f < 0 {
			return fmt.Errorf("cannot convert negative float to uint")
		}
		return setUintChecked(field, uint64(f))
	case string:
		i, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return err
		}
		return setUintChecked(field, i)
	default:
		return fmt.Errorf("cannot convert %T to uint", value)
	}
}

// setBool sets a boolean value to a field
func setBool(field reflect.Value, value any) error {
	switch v := value.(type) {
	case bool:
		field.SetBool(v)
	case string:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return err
		}
		field.SetBool(b)
	case int:
		field.SetBool(v != 0)
	case float64:
		field.SetBool(v != 0)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return err
		}
		field.SetBool(f != 0)
	default:
		return fmt.Errorf("cannot convert %T to bool", value)
	}
	return nil
}

// setFloat sets a floating point value to a field
func setFloat(field reflect.Value, value any) error {
	switch v := value.(type) {
	case float32:
		return setFloatChecked(field, float64(v))
	case float64:
		return setFloatChecked(field, v)
	case int, int8, int16, int32, int64:
		// Use reflection to get the actual int value
		val := reflect.ValueOf(v)
		return setFloatChecked(field, float64(val.Int()))
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return err
		}
		return setFloatChecked(field, f)
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return err
		}
		return setFloatChecked(field, f)
	default:
		return fmt.Errorf("cannot convert %T to float", value)
	}
}

// setSlice sets a slice value to a field
func setSlice(field reflect.Value, value any) error {
	// Repeated form fields, query parameters and headers arrive as []string.
	// Widening them here lets one loop below cover every multi-valued source.
	if strs, ok := value.([]string); ok {
		elems := make([]any, len(strs))
		for i, sv := range strs {
			elems[i] = sv
		}
		value = elems
	}

	if v, ok := value.([]any); ok {
		// Create a new slice with the same type as the field
		s := reflect.MakeSlice(field.Type(), len(v), len(v))

		// Set each element in the slice, reporting every element that fails
		// as BindErrors whose paths are relative to the slice.
		var errs BindErrors
		for i := 0; i < len(v); i++ {
			elem := s.Index(i)
			if elem.Kind() == reflect.Ptr {
				elem.Set(reflect.New(elem.Type().Elem()))
				elem = elem.Elem()
			}

			if err := setField(elem, v[i]); err != nil {
				index := fmt.Sprintf("[%d]", i)
				var inner BindErrors
				if errors.As(err, &inner) {
					errs = append(errs, nestFailures(index, index, "", inner)...)
				} else {
					errs = append(errs, newBindError(index, "", index, err))
				}
			}
		}
		if len(errs) > 0 {
			return errs
		}
		field.Set(s)
		return nil
	}

	// Handle single value that should be converted to a slice
	if field.Type().Elem().Kind() == reflect.String {
		if strVal, ok := value.(string); ok {
			// It's a single string for a string slice
			s := reflect.MakeSlice(field.Type(), 1, 1)
			s.Index(0).SetString(strVal)
			field.Set(s)
			return nil
		}
	}

	return fmt.Errorf("cannot convert %T to slice", value)
}

// setStruct sets a struct value to a field
func setStruct(field reflect.Value, value any) error {
	structMap, ok := value.(map[string]any)
	if !ok {
		if reflect.TypeOf(value).Kind() == reflect.Map {
			// A map of some other key or element type cannot be walked as
			// decoded JSON would be.
			return fmt.Errorf("value mismatch for struct mapping")
		}
		return fmt.Errorf("cannot set struct field with value of type %T", value)
	}
	return bindNestedFields(field, structMap)
}

// isEmptyValue checks if a value is empty or zero
func isEmptyValue(v any) bool {
	if v == nil {
		return true
	}

	// A json.Number is a string underneath, so ask whether it is numerically
	// zero rather than whether it has no characters.
	if n, ok := v.(json.Number); ok {
		f, err := n.Float64()
		return err == nil && f == 0
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String, reflect.Array:
		return rv.Len() == 0
	case reflect.Map, reflect.Slice:
		return rv.IsNil() || rv.Len() == 0
	case reflect.Bool:
		return !rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return rv.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() == 0
	case reflect.Interface, reflect.Ptr:
		return rv.IsNil()
	}
	return false
}
