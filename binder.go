// Package binder provides zero-dependency HTTP request binding for Go.
//
// Binder maps data from HTTP requests to Go structs using struct tags,
// supporting multiple data sources including path parameters, query strings,
// request bodies (JSON and form-encoded), and cookies.
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
// The library is designed to work with Go 1.22+ and its native path parameter support.
// It maintains zero external dependencies and focuses solely on data binding,
// leaving validation and transformation to other specialized tools.
package binder

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
)

// Tag option constants
const (
	optOmitEmpty = "omitempty"
	optRequired  = "required"
)

// bindSources lists the tag sources in precedence order. The first source
// present on a field is the one it binds from.
var bindSources = [...]string{path, query, body, jjson, cookie}

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

// fieldInfo stores cached reflection data for struct fields
type fieldInfo struct {
	Index     int
	FieldType reflect.StructField
	Source    string // "path", "query", "body", "json", "cookie"
	TagName   string
	OmitEmpty bool
}

// Cache for struct field information to improve performance
var fieldCache = make(map[reflect.Type]map[string]fieldInfo)
var fieldCacheMutex sync.RWMutex

// Validator is an optional interface that structs can implement to provide
// custom validation logic that runs automatically after successful binding.
//
// Example:
//
//	type CreateUserRequest struct {
//	    Email string `body:"email"`
//	    Age   int    `body:"age"`
//	}
//
//	func (r CreateUserRequest) Validate() error {
//	    if r.Age < 18 {
//	        return errors.New("user must be 18 or older")
//	    }
//	    return nil
//	}
//
// When a type implements Validator, Bind will call Validate after binding
// and return any validation errors.
type Validator interface {
	Validate() error
}

// DefaultMaxBodySize is the body size limit Bind applies when MaxBodySize has
// not been changed.
const DefaultMaxBodySize int64 = 10 << 20 // 10 MB

// MaxBodySize is the largest request body, in bytes, that Bind will read.
// A larger body is rejected with ErrBodyTooLarge rather than buffered, so that
// a single request cannot exhaust server memory. A value of zero or less
// disables the limit and restores the previous unbounded behaviour.
//
// It is read on every call to Bind, so set it during program initialisation
// rather than while requests are in flight.
var MaxBodySize = DefaultMaxBodySize

// ErrBodyTooLarge is returned by Bind when a request body exceeds MaxBodySize.
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
// A body whose Content-Type is neither JSON nor form-encoded is not parsed at
// all and so is never malformed; such a request binds from its path, query and
// cookie values alone.
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

// BindError describes a failure to bind one field of the target struct. It
// carries the field alongside the source it was read from, so a handler can
// report which input was at fault rather than only that something failed:
//
//	var bindErr *binder.BindError
//	if errors.As(err, &bindErr) {
//	    fmt.Printf("field %s from %s %q: %v\n",
//	        bindErr.Field, bindErr.Source, bindErr.Name, bindErr)
//	}
type BindError struct {
	Field   string // name of the Go struct field
	Source  string // tag source the value was read from, such as "query"
	Name    string // key looked up in that source
	Message string // complete description of what went wrong
	Err     error  // underlying cause, reachable with errors.Is and errors.As
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
	// MaxBodySize overrides the package-level MaxBodySize for this call.
	// Zero leaves the package setting in force, and a negative value removes
	// the limit for this call alone.
	MaxBodySize int64

	// DisallowUnknownFields makes binding fail with ErrUnknownField when the
	// request body carries a top-level key that no field of the target binds.
	// Keys nested inside objects are not inspected.
	DisallowUnknownFields bool
}

// maxBodySize resolves the body limit for a call, falling back to the
// package-level setting when the option is left at its zero value.
func (o BindOptions) maxBodySize() int64 {
	if o.MaxBodySize == 0 {
		return MaxBodySize
	}
	return o.MaxBodySize
}

// Bind maps data from an HTTP request into a struct using reflection and struct tags.
//
// The target must be a pointer to a struct. Bind supports multiple data sources:
//
//   - path:"name"   - URL path parameters (requires Go 1.22+)
//   - query:"name"  - URL query parameters
//   - body:"name"   - Request body (JSON or form-encoded based on Content-Type)
//   - json:"name"   - Alternative to body tag for JSON data
//   - cookie:"name" - HTTP cookies
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
//   - The request body exceeds MaxBodySize (see ErrBodyTooLarge)
//   - The request body cannot be parsed (see ErrMalformedBody)
//   - Validation fails (if the struct implements Validator)
func Bind(r *http.Request, i interface{}) error {
	return BindWithOptions(r, i, BindOptions{})
}

// BindWithOptions is Bind with per-call configuration. Bind is equivalent to
// passing the zero BindOptions.
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
func BindWithOptions(r *http.Request, i interface{}, opts BindOptions) error {
	if r == nil {
		return fmt.Errorf("%w: cannot bind from a nil request", ErrInvalidTarget)
	}

	typ, val, err := targetStruct(i)
	if err != nil {
		return err
	}

	// Parse request body once
	bodyData, err := parseRequestBody(r, opts.maxBodySize())
	if err != nil {
		return err
	}

	if opts.DisallowUnknownFields {
		if err := checkUnknownFields(typ, bodyData); err != nil {
			return err
		}
	}

	// Process each field in the struct
	if err := bindStructFields(r, typ, val, bodyData); err != nil {
		return err
	}

	// Run validation if the struct implements Validator
	if validator, ok := i.(Validator); ok {
		if err := validator.Validate(); err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}
	}

	return nil
}

// targetStruct validates the destination given to Bind and returns the struct
// type and value to bind into. Bind's contract is a non-nil pointer to a
// struct, and anything else is reported as ErrInvalidTarget rather than left
// to panic inside the reflect package.
func targetStruct(i interface{}) (reflect.Type, reflect.Value, error) {
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

// parseRequestBody reads and parses the request body, restoring it for other readers
func parseRequestBody(r *http.Request, maxBodySize int64) (map[string]interface{}, error) {
	// Content-Length is not consulted here: a chunked request declares no
	// length at all, so skipping on a non-positive Content-Length would drop
	// its body entirely. Whether a body is empty is decided after reading.
	if r.Body == nil || r.Body == http.NoBody {
		return make(map[string]interface{}), nil
	}

	// Read the body once, refusing anything oversized
	bodyBytes, err := readBody(r, maxBodySize)
	if err != nil {
		return nil, err
	}

	// Restore the body for other potential readers
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	// An absent body is not a malformed one, so an empty read is reported as
	// no data rather than handed to a parser that would reject it.
	if len(bodyBytes) == 0 {
		return make(map[string]interface{}), nil
	}

	// Create a copy of the request with the new body for parsing
	rCopy := *r
	rCopy.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	// Parse the body. A body that cannot be parsed is reported rather than
	// discarded: binding would otherwise report success while every
	// body-sourced field was silently left at its zero value.
	return parseBody(rCopy)
}

// readBody reads the whole request body, refusing bodies larger than
// MaxBodySize. The limit is enforced while reading rather than trusting
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

// checkUnknownFields reports body keys that no field of the target binds.
// Only top-level keys are considered, since nested values are bound by the
// nested struct rather than by a tag on this one.
func checkUnknownFields(typ reflect.Type, bodyData map[string]interface{}) error {
	if len(bodyData) == 0 {
		return nil
	}

	known := make(map[string]struct{}, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		source, name, _, ok := fieldTag(typ.Field(i))
		if ok && (source == body || source == jjson) {
			known[name] = struct{}{}
		}
	}

	var unknown []string
	for name := range bodyData {
		if _, found := known[name]; !found {
			unknown = append(unknown, strconv.Quote(name))
		}
	}
	if len(unknown) == 0 {
		return nil
	}

	// Sorted so the error does not vary with map iteration order.
	slices.Sort(unknown)
	return fmt.Errorf("%w: %s", ErrUnknownField, strings.Join(unknown, ", "))
}

// bindStructFields processes each field in the struct and binds data from the request
func bindStructFields(r *http.Request, typ reflect.Type, val reflect.Value, bodyData map[string]interface{}) error {
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fieldVal := val.Field(i)

		// Unexported fields cannot be set through reflection, so they are
		// ignored even when tagged, as encoding/json does.
		if !fieldVal.CanSet() {
			continue
		}

		// Extract value from appropriate source
		value, exists, err := extractFieldValue(r, field, bodyData)
		if err != nil {
			return err
		}

		// A missing value is an error when the field is tagged required,
		// and is otherwise simply left at its zero value.
		if !exists {
			if err := checkRequired(field); err != nil {
				return err
			}
			continue
		}

		// Skip if the value should be omitted
		if shouldOmitField(field, value) {
			continue
		}

		// Set the field value
		if err := bindFieldValue(fieldVal, value, field); err != nil {
			return err
		}
	}
	return nil
}

// extractFieldValue gets the value for a field from the appropriate request source
func extractFieldValue(r *http.Request, field reflect.StructField, bodyData map[string]interface{}) (interface{}, bool, error) {
	source, name, _, ok := fieldTag(field)
	if !ok {
		return nil, false, nil
	}

	switch source {
	case path:
		v := r.PathValue(name)
		return v, v != "", nil

	case query:
		v := r.URL.Query().Get(name)
		return v, v != "", nil

	case body, jjson:
		v, exists := bodyData[name]
		return v, exists, nil

	case cookie:
		c, err := r.Cookie(name)
		if err == nil {
			return c.Value, true, nil
		}
		return nil, false, nil

	default:
		return nil, false, nil
	}
}

// checkRequired reports an error when a field tagged with the "required"
// option had no value in its source. For path and query parameters an empty
// value counts as missing, since neither source distinguishes the two.
func checkRequired(field reflect.StructField) error {
	source, name, opts, ok := fieldTag(field)
	if !ok || !hasOption(opts, optRequired) {
		return nil
	}
	return &BindError{
		Field:   field.Name,
		Source:  source,
		Name:    name,
		Message: fmt.Sprintf("missing required field %s: no %s value named %q", field.Name, source, name),
		Err:     ErrMissingRequired,
	}
}

// shouldOmitField determines if a field should be skipped based on omitempty
func shouldOmitField(field reflect.StructField, value interface{}) bool {
	tag := field.Tag
	omitEmpty := strings.Contains(tag.Get(path)+tag.Get(query)+tag.Get(body)+tag.Get(jjson)+tag.Get(cookie), "omitempty")
	return omitEmpty && isEmptyValue(value)
}

// bindFieldValue sets the value on a struct field, handling nested structs and pointers
func bindFieldValue(fieldVal reflect.Value, value interface{}, field reflect.StructField) error {
	fieldName := field.Name
	if fieldVal.Kind() == reflect.Ptr && fieldVal.IsNil() {
		fieldVal.Set(reflect.New(fieldVal.Type().Elem())) // Initialize pointer fields
	}

	// Handle nested structs recursively
	if fieldVal.Kind() == reflect.Struct || (fieldVal.Kind() == reflect.Ptr && fieldVal.Elem().Kind() == reflect.Struct) {
		if nestedMap, ok := value.(map[string]interface{}); ok {
			if err := BindStruct(fieldVal, nestedMap); err != nil {
				return newBindError(field, fmt.Sprintf("error binding nested field %s: %v", fieldName, err), err)
			}
			return nil
		}
	}

	if err := setField(fieldVal, value); err != nil {
		return newBindError(field, fmt.Sprintf("error setting field %s: %v", fieldName, err), err)
	}
	return nil
}

// newBindError builds a BindError carrying the field's binding source, so that
// a caller can report which input was at fault.
func newBindError(field reflect.StructField, message string, err error) *BindError {
	source, name, _, _ := fieldTag(field)
	return &BindError{
		Field:   field.Name,
		Source:  source,
		Name:    name,
		Message: message,
		Err:     err,
	}
}

// getFieldInfo returns cached field information for a struct type
func getFieldInfo(typ reflect.Type) map[string]fieldInfo {
	fieldCacheMutex.RLock()
	info, found := fieldCache[typ]
	fieldCacheMutex.RUnlock()

	if found {
		return info
	}

	// Build field info
	fieldCacheMutex.Lock()
	defer fieldCacheMutex.Unlock()

	// Check again in case another goroutine built it while we were waiting
	if info, found = fieldCache[typ]; found {
		return info
	}

	info = make(map[string]fieldInfo)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fi := fieldInfo{
			Index:     i,
			FieldType: field,
		}

		// Check each tag type
		if tag := field.Tag.Get(path); tag != "" {
			fi.Source = path
			fi.TagName = tag
			fi.OmitEmpty = strings.Contains(tag, "omitempty")
			info[field.Name] = fi
			continue
		}

		if tag := field.Tag.Get(query); tag != "" {
			fi.Source = query
			fi.TagName = tag
			fi.OmitEmpty = strings.Contains(tag, "omitempty")
			info[field.Name] = fi
			continue
		}

		if tag := field.Tag.Get(body); tag != "" {
			fi.Source = body
			fi.TagName = tag
			fi.OmitEmpty = strings.Contains(tag, "omitempty")
			info[field.Name] = fi
			continue
		}

		if tag := field.Tag.Get(jjson); tag != "" {
			fi.Source = jjson
			fi.TagName = tag
			fi.OmitEmpty = strings.Contains(tag, "omitempty")
			info[field.Name] = fi
			continue
		}

		if tag := field.Tag.Get(cookie); tag != "" {
			fi.Source = cookie
			fi.TagName = tag
			fi.OmitEmpty = strings.Contains(tag, "omitempty")
			info[field.Name] = fi
			continue
		}
	}

	fieldCache[typ] = info
	return info
}

// BindStruct recursively binds data from a map to a struct field, handling nested structures.
//
// This function is exported for advanced use cases where you need to bind nested
// data manually. Most users should use Bind instead.
//
// Parameters:
//   - field: The reflect.Value of the struct field to bind to
//   - data: Map containing the data to bind
//
// The function handles both pointer and non-pointer fields, automatically
// initializing nil pointers as needed.
func BindStruct(field reflect.Value, data map[string]interface{}) error {
	target := field
	if field.Kind() == reflect.Ptr {
		if field.IsNil() {
			field.Set(reflect.New(field.Type().Elem()))
		}
		target = field.Elem()
	}

	typ := target.Type()
	for i := 0; i < typ.NumField(); i++ {
		fieldType := typ.Field(i)
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

		nestedField := target.Field(i)
		if nestedField.Kind() == reflect.Ptr && nestedField.IsNil() {
			nestedField.Set(reflect.New(nestedField.Type().Elem()))
		}

		if err := setField(nestedField, nestedValue); err != nil {
			return fmt.Errorf("error setting nested field %s: %w", fieldType.Name, err)
		}
	}
	return nil
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

// parseBody extracts and parses the request body into a map
func parseBody(r http.Request) (map[string]interface{}, error) {
	var reqBody map[string]interface{}
	ct := parseContentType(r.Header.Get("Content-Type"))

	switch ct {
	case "application/json":
		err := json.NewDecoder(r.Body).Decode(&reqBody)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid JSON: %w", ErrMalformedBody, err)
		}
		return reqBody, nil

	case "application/x-www-form-urlencoded":
		reqBody = make(map[string]interface{})
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

	return make(map[string]interface{}), nil
}

// setField sets the appropriate value on the given reflect.Value field
func setField(field reflect.Value, value interface{}) error {
	// Handle nil value
	if value == nil {
		return nil
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
func tryTextUnmarshaler(field reflect.Value, value interface{}) (bool, error) {
	if field.Type().Implements(reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()) {
		strVal, ok := value.(string)
		if !ok {
			return true, errors.New("value is not a string for TextUnmarshaler")
		}
		return true, field.Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(strVal))
	}

	if field.CanAddr() && reflect.PointerTo(field.Type()).Implements(reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()) {
		strVal, ok := value.(string)
		if !ok {
			return true, errors.New("value is not a string for TextUnmarshaler")
		}
		return true, field.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(strVal))
	}

	return false, nil // No TextUnmarshaler interface found
}

// setFieldByKind sets the field value based on its reflect.Kind
func setFieldByKind(field reflect.Value, value interface{}) error {
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
func setString(field reflect.Value, value interface{}) error {
	str, err := toString(value)
	if err != nil {
		return err
	}
	field.SetString(str)
	return nil
}

// toString converts various types to string
func toString(value interface{}) (string, error) {
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

// setInt sets an integer value to a field
func setInt(field reflect.Value, value interface{}) error {
	switch v := value.(type) {
	case int:
		field.SetInt(int64(v))
	case int8:
		field.SetInt(int64(v))
	case int16:
		field.SetInt(int64(v))
	case int32:
		field.SetInt(int64(v))
	case int64:
		field.SetInt(v)
	case float32:
		field.SetInt(int64(v))
	case float64:
		field.SetInt(int64(v))
	case string:
		i, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return err
		}
		field.SetInt(i)
	default:
		return fmt.Errorf("cannot convert %T to int", value)
	}
	return nil
}

// setUint sets an unsigned integer value to a field
func setUint(field reflect.Value, value interface{}) error {
	switch v := value.(type) {
	case uint:
		field.SetUint(uint64(v))
	case uint8:
		field.SetUint(uint64(v))
	case uint16:
		field.SetUint(uint64(v))
	case uint32:
		field.SetUint(uint64(v))
	case uint64:
		field.SetUint(v)
	case int:
		if v < 0 {
			return fmt.Errorf("cannot convert negative int to uint")
		}
		field.SetUint(uint64(v))
	case float64:
		if v < 0 {
			return fmt.Errorf("cannot convert negative float to uint")
		}
		field.SetUint(uint64(v))
	case string:
		i, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return err
		}
		field.SetUint(i)
	default:
		return fmt.Errorf("cannot convert %T to uint", value)
	}
	return nil
}

// setBool sets a boolean value to a field
func setBool(field reflect.Value, value interface{}) error {
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
	default:
		return fmt.Errorf("cannot convert %T to bool", value)
	}
	return nil
}

// setFloat sets a floating point value to a field
func setFloat(field reflect.Value, value interface{}) error {
	switch v := value.(type) {
	case float32:
		field.SetFloat(float64(v))
	case float64:
		field.SetFloat(v)
	case int, int8, int16, int32, int64:
		// Use reflection to get the actual int value
		val := reflect.ValueOf(v)
		field.SetFloat(float64(val.Int()))
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return err
		}
		field.SetFloat(f)
	default:
		return fmt.Errorf("cannot convert %T to float", value)
	}
	return nil
}

// setSlice sets a slice value to a field
func setSlice(field reflect.Value, value interface{}) error {
	if v, ok := value.([]interface{}); ok {
		// Create a new slice with the same type as the field
		s := reflect.MakeSlice(field.Type(), len(v), len(v))

		// Set each element in the slice
		for i := 0; i < len(v); i++ {
			elem := s.Index(i)
			if elem.Kind() == reflect.Ptr {
				elem.Set(reflect.New(elem.Type().Elem()))
				elem = elem.Elem()
			}

			if err := setField(elem, v[i]); err != nil {
				return fmt.Errorf("error setting slice element at index %d: %w", i, err)
			}
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
func setStruct(field reflect.Value, value interface{}) error {
	// Handle map to struct conversion
	if structMap, ok := value.(map[string]interface{}); ok {
		for x := 0; x < field.NumField(); x++ {
			nestedField := field.Field(x)
			nestedStructType := field.Type().Field(x)

			tagValue := nestedStructType.Tag.Get(body)
			if tagValue == "" {
				tagValue = nestedStructType.Tag.Get(jjson)
			}

			if tagValue != "" {
				name, _ := splitTag(tagValue)
				if nestedVal, exists := structMap[name]; exists {
					if err := setField(nestedField, nestedVal); err != nil {
						return fmt.Errorf("error setting nested field '%s': %w", nestedStructType.Name, err)
					}
				}
			}
		}
		return nil
	} else if reflect.TypeOf(value).Kind() == reflect.Map {
		// If not directly map[string]interface{}, handle map or struct assignment gracefully
		return fmt.Errorf("value mismatch for struct mapping")
	}

	return fmt.Errorf("cannot set struct field with value of type %T", value)
}

// isEmptyValue checks if a value is empty or zero
func isEmptyValue(v interface{}) bool {
	if v == nil {
		return true
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
