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
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
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
//
// As in encoding/json, `json:"-"` is not a binding tag, and a tag with an empty
// name, such as `json:",omitempty"`, binds under the Go field name.
func fieldTag(field reflect.StructField, sources []string) (source, name, opts string, ok bool) {
	for _, src := range sources {
		if name, opts, ok = sourceTag(field, src); ok {
			return src, name, opts, true
		}
	}
	return "", "", "", false
}

// taggedField is a field that binds, found on a struct itself or promoted
// from a struct it embeds.
type taggedField struct {
	index  []int  // path from the outer struct, as reflect.Value.FieldByIndex takes
	goName string // Go name, through any embedded structs: Paging.Page
	field  reflect.StructField
	source string
	name   string
	opts   string
}

// taggedFields lists the fields of typ that bind from the given sources, in
// declaration order. As in encoding/json, an embedded struct, or pointer to
// one, that carries no binding tag of its own has its fields promoted; a
// tagged one is an ordinary field. A key declared at more than one depth
// binds only the shallowest field, and at the same depth, the first declared.
func taggedFields(typ reflect.Type, sources []string) []taggedField {
	if !embedsStruct(typ) {
		return flatTaggedFields(typ, sources)
	}

	type level struct {
		typ   reflect.Type
		index []int
		path  string
	}
	var out []taggedField
	seen := make(map[[2]string]bool)
	visited := make(map[reflect.Type]bool)
	for current := []level{{typ: typ}}; len(current) > 0; {
		var next []level
		var found []taggedField
		for _, l := range current {
			// A struct reached twice, as through a cycle of embedded
			// pointers, has already given up its fields.
			if visited[l.typ] {
				continue
			}
			visited[l.typ] = true

			for i := 0; i < l.typ.NumField(); i++ {
				f := l.typ.Field(i)
				index := append(slices.Clip(l.index), i)
				source, name, opts, ok := fieldTag(f, sources)

				// As in encoding/json, `json:"-"` excludes an embedded struct
				// entirely, and one whose tag gives no name, such as
				// `json:",omitempty"`, is still promoted.
				if f.Anonymous && !ok && f.Tag.Get(jjson) == "-" {
					continue
				}
				if f.Anonymous && (!ok || !namedInTag(f, source)) {
					t := f.Type
					isPtr := t.Kind() == reflect.Ptr
					if isPtr {
						t = t.Elem()
					}
					// An unexported embedded pointer cannot be allocated,
					// but an unexported embedded struct's exported fields
					// can still be set.
					if t.Kind() == reflect.Struct {
						if f.IsExported() || !isPtr {
							next = append(next, level{typ: t, index: index, path: l.path + f.Name + "."})
						}
						continue
					}
					// An embedded type that is not a struct has nothing to
					// promote; with a tag it binds as an ordinary field.
					if !ok {
						continue
					}
				}

				// Unexported fields cannot be set through reflection, so they
				// are ignored even when tagged, as encoding/json does.
				if !ok || !f.IsExported() {
					continue
				}
				found = append(found, taggedField{index: index, goName: l.path + f.Name, field: f, source: source, name: name, opts: opts})
			}
		}
		for _, tf := range found {
			key := [2]string{bindKeySource(tf.source), tf.name}
			if !seen[key] {
				seen[key] = true
				out = append(out, tf)
			}
		}
		current = next
	}
	slices.SortFunc(out, func(a, b taggedField) int { return slices.Compare(a.index, b.index) })
	return out
}

// embedsStruct reports whether typ has an embedded field, so that
// taggedFields must take the walk that handles promotion.
func embedsStruct(typ reflect.Type) bool {
	for i := 0; i < typ.NumField(); i++ {
		if f := typ.Field(i); f.Anonymous {
			return true
		}
	}
	return false
}

// flatTaggedFields is taggedFields for a struct that embeds nothing, which is
// most of them. It keeps the walk's maps and sort off the path, with every
// index sharing one backing array, so resolving a new type stays cheap.
func flatTaggedFields(typ reflect.Type, sources []string) []taggedField {
	n := typ.NumField()
	indexes := make([]int, n)
	out := make([]taggedField, 0, n)
	for i := 0; i < n; i++ {
		f := typ.Field(i)
		source, name, opts, ok := fieldTag(f, sources)
		if !ok || !f.IsExported() || declared(out, source, name) {
			continue
		}
		indexes[i] = i
		out = append(out, taggedField{index: indexes[i : i+1 : i+1], goName: f.Name, field: f, source: source, name: name, opts: opts})
	}
	return out
}

// declared reports whether a key is already bound by one of fields. A linear
// scan suits the handful of fields a request type has.
func declared(fields []taggedField, source, name string) bool {
	source = bindKeySource(source)
	for _, tf := range fields {
		if bindKeySource(tf.source) == source && tf.name == name {
			return true
		}
	}
	return false
}

// bindKeySource names the space a key belongs to: body and its json alias
// read the same body members, so `body:"x"` and `json:"x"` are one key.
func bindKeySource(source string) string {
	if source == jjson {
		return body
	}
	return source
}

// fieldByIndex returns the field at index, allocating any nil embedded pointer
// on the way to it, as encoding/json does when it sets a promoted field. It is
// called only once a field has a value to set, not for an absent one or a
// null, so an embedded pointer none of whose fields is sent stays nil.
func fieldByIndex(v reflect.Value, index []int) reflect.Value {
	if len(index) == 1 {
		return v.Field(index[0])
	}
	for i, x := range index {
		if i > 0 && v.Kind() == reflect.Ptr {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(x)
	}
	return v
}

// namedInTag reports whether a field's tag for source spells out a name, as
// `json:"x"` does and `json:",omitempty"` does not.
func namedInTag(f reflect.StructField, source string) bool {
	name, _ := splitTag(f.Tag.Get(source))
	return name != ""
}

// sourceTag returns a field's tag for one source, split into name and options.
func sourceTag(field reflect.StructField, src string) (name, opts string, ok bool) {
	tag := field.Tag.Get(src)
	if tag == "" || (src == jjson && tag == "-") {
		return "", "", false
	}
	name, opts = splitTag(tag)
	if name == "" {
		name = field.Name
	}
	return name, opts, true
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
	Index     []int               // path to the field, through any embedded structs
	Name      string              // Go name for error reporting, such as Paging.Page
	FieldType reflect.StructField // the field itself
	Source    string              // "path", "query", "body", "json", "cookie", "header"
	TagName   string              // key to look up in Source, without options
	OmitEmpty bool
	Required  bool
	IsSlice   bool     // destination is a slice, so repeated values all bind
	IsMap     bool     // destination is a map, filled from name[key]=value pairs
	Fast      fastKind // set straight from a JSON token, skipping conversion
	// JSON is set when the field's type decodes itself from JSON, so a body
	// member can be handed to it as the raw bytes the client sent.
	JSON bool
	// HeaderKey is TagName in canonical form, for a header field. Resolving
	// it once lets r.Header be indexed directly, where Header.Get
	// canonicalises the name, allocating, on every request.
	HeaderKey string
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
	// bodyMaps names the body fields that are maps, whose members a form body
	// sends as name[key] fields.
	bodyMaps map[string]struct{}
	// bodyFields indexes fields by the body member they bind, so a token walk
	// can find the destination without a second pass.
	bodyFields map[string]int
	// validatePath is the path to the embedded struct a Validate method is
	// promoted from, when the type does not declare its own; nil otherwise.
	validatePath []int
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
// still reach them. If Validate returns ctx.Err(), or an error wrapping it, a
// cancelled request matches context.Canceled.
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
// pointer to a struct, or the request is nil. Unlike ErrBodyTooLarge and ErrMalformedBody it reports
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
	// value, such as address.postcode or tags[2]. It is set for every failure
	// but an unknown body member whose name was itself empty.
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

	// DisallowUnknownFields reports each top-level body key that no field of
	// the target binds, as a BindErrors entry wrapping ErrUnknownField. Keys
	// nested inside objects are not inspected.
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
// The body is read only when a field binds from it, or when
// BindOptions.DisallowUnknownFields needs to see it, and is restored afterwards
// so a later handler can read it again.
//
// Returns an error if:
//   - The target is not a non-nil pointer to a struct, or the request is nil
//     (see ErrInvalidTarget)
//   - Type conversion fails
//   - Required fields are missing
//   - The request body exceeds DefaultMaxBodySize (see ErrBodyTooLarge)
//   - The request body cannot be parsed (see ErrMalformedBody)
//   - The request body cannot be read, such as when the client disconnects
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

	// Which fields the JSON walk filled, so the rest can be bound from other
	// sources. A struct of up to 64 fields, which is nearly every request
	// type, records them in an array on the stack rather than in a slice
	// allocated per call.
	var boundArr [64]bool
	var bound []bool
	if len(info.fields) <= len(boundArr) {
		bound = boundArr[:len(info.fields)]
	} else {
		bound = make([]bool, len(info.fields))
	}

	// Parse request body once, and only when something reads it: a target
	// with no body field leaves the body unread for the handler, unless
	// unknown members must be reported, when every member is one.
	var bodyData map[string]any
	var unknown []string
	if len(info.bodyKeys) > 0 || opts.DisallowUnknownFields {
		bodyData, unknown, err = parseRequestBody(r, opts.maxBodySize(), wanted, info, val, opts.DisallowUnknownFields, bound, &errs)
		if err != nil {
			return err
		}
	}

	// Process each field in the struct
	bindStructFields(r, info, val, bodyData, bound, &errs)

	if opts.DisallowUnknownFields {
		if bodyData != nil {
			unknown = unknownKeys(info, bodyData)
		}
		errs.unknown = unknownFieldErrors(unknown)
	}

	// Validate runs only on a fully bound target: a field that failed holds
	// whatever was left in it, and rules checking it would report a second,
	// misleading failure for the same input.
	if err := errs.list(); err != nil {
		return err
	}
	return validate(r.Context(), i, val, info.validatePath)
}

// BindErrors is the error Bind returns when any field fails to bind, whether
// one or several: every field is attempted, so a client can correct all of its
// input in one round trip. Entries are in the order the struct declares its
// fields, followed by any unknown body members.
//
//	var errs binder.BindErrors
//	if errors.As(err, &errs) {
//	    for _, e := range errs {
//	        if errors.Is(e, binder.ErrMissingRequired) {
//	            problems[e.Name] = "required"
//	        } else {
//	            problems[e.Name] = "invalid value"
//	        }
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
func validate(ctx context.Context, i any, val reflect.Value, promotedFrom []int) error {
	v, ok := i.(Validator)
	if !ok {
		return nil
	}
	// A Validate promoted from an embedded pointer that was never allocated,
	// because none of its fields was sent, has nothing to validate, and
	// calling it would dereference the nil pointer.
	if throughNilPointer(val, promotedFrom) {
		return nil
	}
	if err := v.Validate(ctx); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	return nil
}

// throughNilPointer reports whether the path from val to an embedded field
// passes through a nil pointer, the field itself included.
func throughNilPointer(val reflect.Value, index []int) bool {
	for _, x := range index {
		val = val.Field(x)
		if val.Kind() == reflect.Ptr {
			if val.IsNil() {
				return true
			}
			val = val.Elem()
		}
	}
	return false
}

// validatorType is the Validator interface, for finding where a type's
// Validate method comes from.
var validatorType = reflect.TypeFor[Validator]()

// promotedValidatePath returns the path to the embedded struct that typ's
// Validate method is promoted from, or nil when typ declares Validate itself
// or has none. Go's own selector rule decides: the shallowest embedded field
// whose type declares the method.
func promotedValidatePath(typ reflect.Type) []int {
	if !reflect.PointerTo(typ).Implements(validatorType) || declaresMethod(typ, "Validate") {
		return nil
	}
	type level struct {
		typ   reflect.Type
		index []int
	}
	for current := []level{{typ: typ}}; len(current) > 0; {
		var next []level
		for _, l := range current {
			for i := 0; i < l.typ.NumField(); i++ {
				f := l.typ.Field(i)
				if !f.Anonymous {
					continue
				}
				t := f.Type
				if t.Kind() == reflect.Ptr {
					t = t.Elem()
				}
				if t.Kind() != reflect.Struct {
					continue
				}
				index := append(slices.Clip(l.index), i)
				if declaresMethod(t, "Validate") {
					return index
				}
				next = append(next, level{typ: t, index: index})
			}
		}
		current = next
	}
	return nil
}

// declaresMethod reports whether a type declares a method itself, on a value
// or pointer receiver, rather than having it promoted from a field it embeds.
// reflect lists both alike; what tells them apart is that the compiler
// generates a wrapper for a promoted method, which the runtime reports as
// <autogenerated>. When that cannot be told, the method is taken as declared,
// so Validate is called as it always was.
func declaresMethod(t reflect.Type, name string) bool {
	for _, tt := range [...]reflect.Type{t, reflect.PointerTo(t)} {
		m, ok := tt.MethodByName(name)
		if !ok {
			continue
		}
		pc := m.Func.Pointer()
		fn := runtime.FuncForPC(pc)
		if fn == nil {
			return true
		}
		if file, _ := fn.FileLine(pc); file != "<autogenerated>" {
			return true
		}
	}
	return false
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
// A JSON body is bound straight into the target, which returns a nil map, the set of fields it filled and, when wantUnknown is
// set, the members nothing binds; a field it could not fill is recorded in
// errs. Every other format returns the map that binding reads from, and a nil
// set.
func parseRequestBody(r *http.Request, maxBodySize int64, wanted map[string]struct{}, info *typeInfo, val reflect.Value, wantUnknown bool, bound []bool, errs *fieldErrs) (map[string]any, []string, error) {
	// Content-Length is not consulted here: a chunked request declares no
	// length at all, so skipping on a non-positive Content-Length would drop
	// its body entirely. Whether a body is empty is decided after reading.
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil, nil
	}

	// Read the body once, refusing anything oversized. A read that fails
	// part way puts back what it consumed ahead of the rest, so the body a
	// later handler sees is the one the client sent.
	bodyBytes, err := readBody(r, maxBodySize)
	if err != nil {
		if len(bodyBytes) > 0 {
			r.Body = &prefixedBody{Reader: io.MultiReader(bytes.NewReader(bodyBytes), r.Body), Closer: r.Body}
		}
		return nil, nil, err
	}

	// Restore the body for other potential readers
	r.Body = newReplayBody(bodyBytes)

	// An absent body is not a malformed one, so an empty read is reported as
	// no data rather than handed to a parser that would reject it.
	if len(bodyBytes) == 0 {
		return nil, nil, nil
	}

	// A JSON body is handled before the other formats: its members go
	// straight into their fields rather than through a map.
	if isJSONContentType(parseContentType(r.Header.Get("Content-Type"))) {
		// A failure to convert one member concerns that field and is
		// recorded in errs; only a failure to read the body is returned.
		bodyData, unknown, err := jsonBodyInto(bodyBytes, info, val, wanted, wantUnknown, bound, errs)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: invalid JSON: %w", ErrMalformedBody, err)
		}
		return bodyData, unknown, nil
	}

	// Parse the body. A body that cannot be parsed is reported rather than
	// discarded: binding would otherwise report success while every
	// body-sourced field was silently left at its zero value.
	bodyData, err := parseBody(r.Header.Get("Content-Type"), bodyBytes)
	return bodyData, nil, err
}

// replayBody is a request body restored from bytes already read, so that a
// handler or middleware after Bind can read it again. It is one allocation,
// where io.NopCloser around a bytes.Buffer is two.
type replayBody struct{ bytes.Reader }

func (*replayBody) Close() error { return nil }

// prefixedBody is a request body whose first bytes were read and then put
// back, closing the original body when it is closed.
type prefixedBody struct {
	io.Reader
	io.Closer
}

func newReplayBody(b []byte) *replayBody {
	rb := new(replayBody)
	rb.Reset(b)
	return rb
}

// maxPresize is the most readBody allocates before any body has arrived.
const maxPresize = 64 << 10

// readBody reads the whole request body, refusing bodies larger than limit,
// or reading without bound when limit is zero or less. The limit is enforced
// while reading rather than trusting Content-Length, which the client
// controls and may understate. An oversized body is reported as an error
// rather than truncated, so that a request is never bound from a partial
// body. On failure it still returns the bytes it consumed, so that the caller
// can put them back.
//
// It reads into one buffer rather than using io.ReadAll over io.LimitReader:
// that saves the LimitReader, and a buffer sized from Content-Length, where
// one is declared within the limit, is a single allocation of the right size
// rather than 512 bytes grown by doubling. An understated length only means
// the buffer grows; it cannot get past the limit.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	// Reject an honestly declared oversized body without reading it at all.
	if limit > 0 && r.ContentLength > limit {
		return nil, fmt.Errorf("%w: %d bytes declared, limit is %d", ErrBodyTooLarge, r.ContentLength, limit)
	}

	// One byte more than the declared length leaves room to see EOF without
	// growing. The length is the client's claim, so it sizes the buffer only
	// up to maxPresize: a request declaring a large body and sending nothing
	// must not make the server allocate the whole limit up front. A larger
	// body grows the buffer as it arrives.
	size := 512
	if limit > 0 && r.ContentLength > 0 {
		size = int(min(r.ContentLength, maxPresize)) + 1
	}
	buf := make([]byte, 0, size)

	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)] // grow, as io.ReadAll does
		}
		// Read at most one byte past the limit, which is enough to tell an
		// oversized body from one exactly at the limit.
		space := buf[len(buf):cap(buf)]
		// The comparison is arranged so that no sum can overflow, whatever the
		// limit: room+1 is formed only once room is known to be small.
		if room := limit - int64(len(buf)); limit > 0 && room < int64(len(space))-1 {
			space = space[:room+1]
		}
		n, err := r.Body.Read(space)
		buf = buf[:len(buf)+n]
		if limit > 0 && int64(len(buf)) > limit {
			return buf, fmt.Errorf("%w: limit is %d bytes", ErrBodyTooLarge, limit)
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return buf, fmt.Errorf("error reading request body: %w", err)
		}
	}
}

// unknownKeys returns the body keys that no field of the target binds. Only
// top-level keys are considered, since nested values are bound by the nested
// struct rather than by a tag on this one.
func unknownKeys(info *typeInfo, bodyData map[string]any) []string {
	var unknown []string
	for name := range bodyData {
		if _, found := info.bodyKeys[name]; found {
			continue
		}
		// A form field such as meta[a] belongs to a map field named meta.
		if i := strings.IndexByte(name, '['); i > 0 {
			if _, found := info.bodyMaps[name[:i]]; found {
				if _, ok := bracketKey(name, name[:i]); ok {
					continue
				}
			}
		}
		unknown = append(unknown, name)
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

// queryCache reads query parameters for one call to Bind.
//
// A short query is scanned for each parameter a field asks for, rather than
// parsed into url.Values: building that map, with a slice per key, cost
// several allocations to read a handful of values, where a scan costs none
// for a value without escapes, which is a substring of RawQuery. A long query
// is parsed once into url.Values instead, so that it costs one pass rather
// than one per field, and so that net/url's own limit on the number of
// parameters, urlmaxqueryparams, applies to it unchanged.
type queryCache struct {
	url     *url.URL
	checked bool // whether short has been decided
	short   bool
	parsed  url.Values
}

// maxScanPairs is the most parameters a query may have for scanning, which
// costs one pass per field, to be cheaper than parsing it once.
const maxScanPairs = 64

func (q *queryCache) scan() bool {
	if !q.checked {
		q.short = strings.Count(q.url.RawQuery, "&") < maxScanPairs
		q.checked = true
	}
	return q.short
}

func (q *queryCache) values() url.Values {
	if q.parsed == nil {
		q.parsed = q.url.Query()
	}
	return q.parsed
}

// get returns the first value of a parameter, as url.Values.Get does.
func (q *queryCache) get(name string) string {
	if !q.scan() {
		return q.values().Get(name)
	}
	for query := q.url.RawQuery; query != ""; {
		var value string
		var ok bool
		value, query, ok = nextQueryValue(query, name)
		if ok {
			return value
		}
	}
	return ""
}

// all returns every value given for a parameter, for binding into a slice.
func (q *queryCache) all(name string) []string {
	if !q.scan() {
		return q.values()[name]
	}
	var values []string
	for query := q.url.RawQuery; query != ""; {
		var value string
		var ok bool
		value, query, ok = nextQueryValue(query, name)
		if ok {
			values = append(values, value)
		}
	}
	return values
}

// group gathers the name[key]=value pairs of a query into a map for binding
// into a map field: ?filter[status]=open&filter[tag]=a&filter[tag]=b gives
// {"status": "open", "tag": ["a", "b"]}, the shape a form body produces. It
// is the bracket form OpenAPI calls deepObject. An empty value counts as
// absent, as elsewhere in the query, and a pair the query parser would skip
// is skipped.
func (q *queryCache) group(name string) map[string]any {
	var out map[string]any
	if !q.scan() {
		for key, vs := range q.values() {
			if sub, ok := bracketKey(key, name); ok {
				for _, v := range vs {
					out = addGrouped(out, sub, v)
				}
			}
		}
		return out
	}
	for query := q.url.RawQuery; query != ""; {
		var pair string
		pair, query, _ = strings.Cut(query, "&")
		if pair == "" || strings.Contains(pair, ";") {
			continue
		}
		key, value, _ := strings.Cut(pair, "=")
		key, err := url.QueryUnescape(key)
		if err != nil {
			continue
		}
		sub, ok := bracketKey(key, name)
		if !ok {
			continue
		}
		if value, err = url.QueryUnescape(value); err != nil {
			continue
		}
		out = addGrouped(out, sub, value)
	}
	return out
}

// groupFormValues gathers a form body's name[key]=value fields into a map, as
// group does for a query. File parts are not text, and are left out.
func groupFormValues(bodyData map[string]any, name string) map[string]any {
	var out map[string]any
	for key, v := range bodyData {
		sub, ok := bracketKey(key, name)
		if !ok {
			continue
		}
		switch v := v.(type) {
		case string:
			out = addGrouped(out, sub, v)
		case []string:
			for _, s := range v {
				out = addGrouped(out, sub, s)
			}
		}
	}
	return out
}

// bracketKey reports whether key is name[sub] and returns sub. Only one level
// is recognised: name[a][b] has no agreed meaning, so it is not matched.
func bracketKey(key, name string) (string, bool) {
	if len(key) < len(name)+3 || !strings.HasPrefix(key, name) || key[len(name)] != '[' || key[len(key)-1] != ']' {
		return "", false
	}
	sub := key[len(name)+1 : len(key)-1]
	if strings.ContainsAny(sub, "[]") {
		return "", false
	}
	return sub, true
}

// addGrouped adds one value under a key, keeping a repeated key's values in a
// []string as a repeated form field is kept. An empty value is skipped.
func addGrouped(out map[string]any, key, value string) map[string]any {
	if value == "" {
		return out
	}
	if out == nil {
		out = make(map[string]any)
	}
	switch prev := out[key].(type) {
	case nil:
		out[key] = value
	case string:
		out[key] = []string{prev, value}
	case []string:
		out[key] = append(prev, value)
	}
	return out
}

// nextQueryValue consumes the next pair from query and reports whether it
// gives a value for name, returning the rest of the query. It skips exactly
// the pairs url.ParseQuery skips, in the same order of checks: an empty one,
// one containing a semicolon, and one whose key or value is not validly
// escaped. QueryUnescape returns its argument unchanged, without allocating,
// when there is nothing to unescape.
//
// Checking the key before the rest of the pair looks cheaper, since most pairs
// are for another parameter, but measured slower: strings.ContainsAny to find
// a key needing no unescape costs more than QueryUnescape's own tight loop.
func nextQueryValue(query, name string) (value, rest string, ok bool) {
	pair, rest, _ := strings.Cut(query, "&")
	if pair == "" || strings.Contains(pair, ";") {
		return "", rest, false
	}
	key, value, _ := strings.Cut(pair, "=")
	key, err := url.QueryUnescape(key)
	if err != nil || key != name {
		return "", rest, false
	}
	value, err = url.QueryUnescape(value)
	if err != nil {
		return "", rest, false
	}
	return value, rest, true
}

// bindStructFields processes each bindable field in the struct and binds data
// from the request, recording in errs each field that fails.
func bindStructFields(r *http.Request, info *typeInfo, val reflect.Value, bodyData map[string]any, bound []bool, errs *fieldErrs) {
	queries := queryCache{url: r.URL}

	for index, fi := range info.fields {
		// A body field the JSON walk already filled needs nothing further.
		if bound[index] {
			continue
		}

		// A single value from the path, query, a header or a cookie is a
		// string, and is set without passing through any: storing a string
		// in an interface puts it on the heap, one allocation per field.
		if isStringSource(fi) {
			s, exists := stringValue(r, fi, &queries)
			if !exists {
				if fi.Required {
					errs.set(info, index, []*BindError{missingRequiredError(fi)})
				}
				continue
			}
			if fi.OmitEmpty && s == "" {
				continue
			}
			if err := setFromString(fieldByIndex(val, fi.Index), s, fi); err != nil {
				errs.set(info, index, fieldFailures(fi, err))
			}
			continue
		}

		// Extract value from appropriate source
		value, exists, err := extractFieldValue(r, fi, bodyData, &queries)
		if err != nil {
			errs.set(info, index, fieldFailures(fi, err))
			continue
		}

		// A missing value is an error when the field is tagged required,
		// and otherwise leaves the field as it was.
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
		if err := bindFieldValue(fieldByIndex(val, fi.Index), value); err != nil {
			errs.set(info, index, fieldFailures(fi, err))
		}
	}
}

// isStringSource reports whether a field binds from a single string: any
// path or cookie field, and a query or header field that is not a slice, which
// takes only the first value.
func isStringSource(fi fieldInfo) bool {
	switch fi.Source {
	case path, cookie:
		return true
	case query, header:
		return !fi.IsSlice && !fi.IsMap
	default:
		return false
	}
}

// stringValue gets the value for a field that binds from a single string, and
// whether the request carried one. For path, query and header an empty value
// counts as absent: path and query cannot tell the two apart, and a header is
// treated the same way.
func stringValue(r *http.Request, fi fieldInfo, queries *queryCache) (string, bool) {
	switch fi.Source {
	case path:
		v := r.PathValue(fi.TagName)
		return v, v != ""

	case query:
		v := queries.get(fi.TagName)
		return v, v != ""

	case cookie:
		return cookieValue(r, fi.TagName)

	case header:
		// HeaderKey is canonical, so `header:"x-request-id"` and
		// `header:"X-Request-ID"` name the same header, as with Header.Get.
		vs := r.Header[fi.HeaderKey]
		if len(vs) == 0 {
			return "", false
		}
		return vs[0], vs[0] != ""

	default:
		return "", false
	}
}

// maxScanCookies is the most cookies a request may carry for cookieValue to
// scan them itself rather than defer to r.Cookie.
const maxScanCookies = 64

// cookieValue returns the value of the named cookie, and whether the request
// carried a valid one, exactly as r.Cookie does. r.Cookie parses every cookie
// in the header into a *Cookie on each call, allocating; this finds the one
// wanted, and returns its value as a substring of the header.
//
// It takes the same steps as net/http's readCookies, in the same order, and
// takes the first valid match. A request with more than maxScanCookies
// cookies is handed to r.Cookie itself, so that net/http's own limit on the
// number of cookies, httpcookiemaxnum, applies to it unchanged.
func cookieValue(r *http.Request, name string) (string, bool) {
	lines := r.Header["Cookie"]
	count := 0
	for _, line := range lines {
		count += strings.Count(line, ";") + 1
	}
	if count > maxScanCookies {
		c, err := r.Cookie(name)
		if err != nil {
			return "", false
		}
		return c.Value, true
	}

	for _, line := range lines {
		line = textproto.TrimString(line)
		for len(line) > 0 {
			var part string
			part, line, _ = strings.Cut(line, ";")
			part = textproto.TrimString(part)
			if part == "" {
				continue
			}
			key, value, _ := strings.Cut(part, "=")
			key = textproto.TrimString(key)
			if key != name || !isCookieName(key) {
				continue
			}
			if value, ok := cookieValueBytes(value); ok {
				return value, true
			}
		}
	}
	return "", false
}

// isCookieName reports whether a cookie name is a token as RFC 7230 defines
// one, which is what net/http requires.
func isCookieName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// cookieValueBytes strips one pair of surrounding double quotes from a raw
// cookie value and reports whether what remains is a valid value, as
// net/http's parseCookieValue does for a request.
func cookieValueBytes(raw string) (string, bool) {
	if len(raw) > 1 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		raw = raw[1 : len(raw)-1]
	}
	for i := 0; i < len(raw); i++ {
		if b := raw[i]; b < 0x20 || b >= 0x7f || b == '"' || b == ';' || b == '\\' {
			return "", false
		}
	}
	return raw, true
}

// setFromString sets a field from a string. A predeclared destination is
// parsed directly; anything else, such as a TextUnmarshaler or a pointer,
// takes the general path, exactly as a value from the body would.
func setFromString(field reflect.Value, s string, fi fieldInfo) error {
	switch fi.Fast {
	case fastString:
		field.SetString(s)
		return nil
	case fastInt:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		return setIntChecked(field, n)
	case fastUint:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return err
		}
		return setUintChecked(field, n)
	case fastFloat:
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		return setFloatChecked(field, n)
	case fastBool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		field.SetBool(b)
		return nil
	default:
		return bindFieldValue(field, s)
	}
}

// extractFieldValue gets the value for a field that isStringSource does not
// cover: a body member, or every value of a repeated query parameter or
// header for a slice.
func extractFieldValue(r *http.Request, fi fieldInfo, bodyData map[string]any, queries *queryCache) (any, bool, error) {
	switch fi.Source {
	case query:
		if fi.IsMap {
			m := queries.group(fi.TagName)
			return m, len(m) > 0, nil
		}
		vs := queries.all(fi.TagName)
		return vs, anyNonEmpty(vs), nil

	case body, jjson:
		v, exists := bodyData[fi.TagName]
		// A form body spells a map as name[key]=value pairs, as a query does.
		if !exists && fi.IsMap {
			if m := groupFormValues(bodyData, fi.TagName); len(m) > 0 {
				return m, true, nil
			}
		}
		return v, exists, nil

	case header:
		vs := r.Header[fi.HeaderKey]
		return vs, anyNonEmpty(vs), nil

	default:
		return nil, false, nil
	}
}

// anyNonEmpty reports whether a query parameter or header given for a slice
// carries a value. Only empty values, such as ?tags=, count as absent, just
// as an empty value does for a field that takes one.
func anyNonEmpty(vs []string) bool {
	for _, v := range vs {
		if v != "" {
			return true
		}
	}
	return false
}

// missingRequiredError reports a field tagged with the "required" option that
// had no value in its source. For path, query and header values an empty value
// counts as missing; an empty body value or cookie satisfies required.
func missingRequiredError(fi fieldInfo) *BindError {
	return &BindError{
		Field:   fi.Name,
		Source:  fi.Source,
		Name:    fi.TagName,
		Message: fmt.Sprintf("missing required field %s: no %s value named %q", fi.Name, fi.Source, fi.TagName),
		Err:     ErrMissingRequired,
	}
}

// bindFieldValue sets the value on a struct field, handling nested structs and
// pointers.
func bindFieldValue(fieldVal reflect.Value, value any) error {
	// A JSON null sets nothing, so a pointer stays nil and "sent as null"
	// stays distinguishable from "sent as zero".
	if value == nil {
		return nil
	}
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
		return nestFailures(fi.Name, fi.TagName, fi.Source, inner)
	}
	return []*BindError{newBindError(fi.Name, fi.Source, fi.TagName, err)}
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

	tagged := taggedFields(typ, bindSources[:])
	info := make([]fieldInfo, 0, len(tagged))
	for _, tf := range tagged {
		field, source, name, opts := tf.field, tf.source, tf.name, tf.opts

		// A pointer to a slice takes many values just as a slice does.
		fieldType := field.Type
		if fieldType.Kind() == reflect.Ptr {
			fieldType = fieldType.Elem()
		}

		var headerKey string
		if source == header {
			headerKey = http.CanonicalHeaderKey(name)
		}

		info = append(info, fieldInfo{
			Index:     tf.index,
			Name:      tf.goName,
			FieldType: field,
			Source:    source,
			TagName:   name,
			OmitEmpty: omitsEmpty(field.Type, opts),
			Required:  hasOption(opts, optRequired),
			IsSlice:   fieldType.Kind() == reflect.Slice && !isTextUnmarshaler(fieldType) && !unmarshalsJSON(fieldType),
			IsMap:     fieldType.Kind() == reflect.Map && !isTextUnmarshaler(fieldType) && !unmarshalsJSON(fieldType),
			Fast:      fastKindOf(field.Type),
			JSON:      unmarshalsJSON(field.Type),
			HeaderKey: headerKey,
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

	var maps map[string]struct{}
	for _, fi := range info {
		if fi.IsMap && (fi.Source == body || fi.Source == jjson) {
			if maps == nil {
				maps = make(map[string]struct{})
			}
			maps[fi.TagName] = struct{}{}
		}
	}

	cached = &typeInfo{fields: info, bodyKeys: keys, bodyFields: fields, bodyMaps: maps, validatePath: promotedValidatePath(typ)}
	fieldCache[typ] = cached
	return cached
}

// omitsEmpty reports whether a field skips an empty value. omitempty has no
// effect on a pointer: the pointer already tells "not sent" (nil) from a zero
// value, and skipping a JSON false or 0 would silently lose the update the
// pointer exists to carry.
func omitsEmpty(t reflect.Type, opts string) bool {
	return t.Kind() != reflect.Ptr && hasOption(opts, optOmitEmpty)
}

// isTextUnmarshaler reports whether a type, or a pointer to it, unmarshals
// itself from text. Such a type takes one value even when its kind is a slice,
// as net.IP's is.
func isTextUnmarshaler(t reflect.Type) bool {
	return t.Implements(textUnmarshalerType) || reflect.PointerTo(t).Implements(textUnmarshalerType)
}

// nestedSources are the tags a nested struct's fields bind from: a nested
// value comes from the body, so only body and its json alias apply.
var nestedSources = []string{body, jjson}

// nestedFieldCache holds the fields of each nested struct type, resolved once.
var nestedFieldCache sync.Map // reflect.Type -> []taggedField

// nestedFieldsFor returns the fields of a nested struct type that bind, with
// those of untagged embedded structs promoted as at the top level.
func nestedFieldsFor(typ reflect.Type) []taggedField {
	if cached, ok := nestedFieldCache.Load(typ); ok {
		return cached.([]taggedField)
	}
	fields := taggedFields(typ, nestedSources)
	nestedFieldCache.Store(typ, fields)
	return fields
}

// bindNestedFields binds map data into a struct's fields, matching each on its
// body tag or the json alias. It is the struct case of setField.
//
// Every field is attempted, and the failures are returned as BindErrors whose
// paths are relative to target.
func bindNestedFields(target reflect.Value, data map[string]any) error {
	var errs BindErrors
	for _, tf := range nestedFieldsFor(target.Type()) {
		name, opts := tf.name, tf.opts

		// required and omitempty mean inside a nested struct what they mean
		// at the top level.
		nestedValue, ok := data[name]
		if !ok {
			if hasOption(opts, optRequired) {
				errs = append(errs, &BindError{
					Field:   tf.goName,
					Source:  body,
					Name:    name,
					Message: fmt.Sprintf("missing required field %s: no %s value named %q", tf.goName, body, name),
					Err:     ErrMissingRequired,
				})
			}
			continue
		}
		if omitsEmpty(tf.field.Type, opts) && isEmptyValue(nestedValue) {
			continue
		}
		// A null sets nothing, so it must not allocate an embedded pointer on
		// the way to its field either.
		if nestedValue == nil {
			continue
		}

		if err := setField(fieldByIndex(target, tf.index), nestedValue); err != nil {
			var inner BindErrors
			if errors.As(err, &inner) {
				errs = append(errs, nestFailures(tf.goName, name, body, inner)...)
			} else {
				errs = append(errs, newBindError(tf.goName, body, name, err))
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
	// Cut walks the parameters without building a slice of them, which
	// strings.Split would allocate on every request.
	for rest := header; ; {
		part, next, more := strings.Cut(rest, ";")
		part = strings.TrimSpace(part)
		if !strings.Contains(part, "=") {
			return strings.ToLower(part)
		}
		if !more {
			return ""
		}
		rest = next
	}
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
func parseBody(contentType string, bodyBytes []byte) (map[string]any, error) {
	var reqBody map[string]any
	ct := parseContentType(contentType)

	switch {
	case ct == "multipart/form-data":
		reqBody, err := parseMultipartBody(contentType, bodyBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid multipart form: %w", ErrMalformedBody, err)
		}
		return reqBody, nil

	case ct == "application/x-www-form-urlencoded":
		// The body is parsed from the bytes already read, not through
		// Request.ParseForm: that applies its own 10 MB cap whatever
		// MaxBodySize says, reads a body only for POST, PUT and PATCH, and
		// fails on a malformed URL query as if the body were at fault.
		form, err := url.ParseQuery(string(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("%w: invalid form data: %w", ErrMalformedBody, err)
		}
		reqBody = make(map[string]any, len(form))
		for k, v := range form {
			if len(v) == 1 {
				reqBody[k] = v[0]
			} else {
				reqBody[k] = v
			}
		}
		return reqBody, nil
	}

	return nil, nil
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

	// A repeated form field arrives as []string. A field that takes one value
	// binds the first, as a repeated query parameter or header does.
	if strs, ok := value.([]string); ok && len(strs) > 0 && takesOneValue(field.Type()) {
		value = strs[0]
	}

	// A type that decodes itself from JSON does so, as with encoding/json,
	// except that a string goes to UnmarshalText when the type has it too, so
	// a value reads the same from a JSON body as from a query string or form.
	if unmarshalsJSON(field.Type()) {
		if _, isString := value.(string); !isString || !isTextUnmarshaler(field.Type()) {
			return unmarshalJSONValue(field, value)
		}
	}

	// Handle TextUnmarshaler interface
	handled, err := tryTextUnmarshaler(field, value)
	if handled {
		return err
	}

	// Handle based on field kind
	return setFieldByKind(field, value)
}

// jsonUnmarshalerTypes are the interfaces through which a type decodes itself
// from JSON: encoding/json's, and json/v2's streaming form.
var jsonUnmarshalerTypes = [...]reflect.Type{
	reflect.TypeFor[json.Unmarshaler](),
	reflect.TypeFor[jsonv2.UnmarshalerFrom](),
}

// unmarshalsJSONCache holds unmarshalsJSON's answer per type: setField asks
// for every value it sets, and Implements is too slow to ask each time.
var unmarshalsJSONCache sync.Map // reflect.Type -> bool

// unmarshalsJSON reports whether a type, or a pointer to it, decodes itself
// from JSON.
func unmarshalsJSON(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Interface:
		return false
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		// A predeclared type has no methods, and is the common case.
		if t.PkgPath() == "" {
			return false
		}
	case reflect.Slice, reflect.Map, reflect.Array:
		// Nor does an unnamed one such as []string.
		if t.Name() == "" {
			return false
		}
	}
	if cached, ok := unmarshalsJSONCache.Load(t); ok {
		return cached.(bool)
	}
	found := false
	for _, u := range jsonUnmarshalerTypes {
		if t.Implements(u) || reflect.PointerTo(t).Implements(u) {
			found = true
			break
		}
	}
	unmarshalsJSONCache.Store(t, found)
	return found
}

// unmarshalJSONValue hands a decoded value to a type's own JSON decoding. A
// value inside a nested struct, slice or map was decoded on the way in, so it
// is encoded again first; numbers are json.Number and keep their digits.
func unmarshalJSONValue(field reflect.Value, value any) error {
	// An Encoder, unlike Marshal, can leave <, > and & as they were sent.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return err
	}
	return unmarshalJSONRaw(field, bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

// unmarshalJSONRaw hands raw JSON to a type's own decoding. json/v2 calls
// UnmarshalJSON or UnmarshalJSONFrom, whichever the type has.
func unmarshalJSONRaw(field reflect.Value, raw []byte) error {
	if !field.CanAddr() {
		return fmt.Errorf("cannot decode JSON into unaddressable %s", field.Type())
	}
	// The body walk accepts duplicate member names, so a value it accepted
	// must not be refused here for the same reason.
	return jsonv2.Unmarshal(raw, field.Addr().Interface(), decodeOptions)
}

// takesOneValue reports whether a destination binds a single value rather than
// one per element: anything but a slice, or a slice that unmarshals itself from
// text. A pointer is judged by what it points to.
func takesOneValue(t reflect.Type) bool {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Kind() != reflect.Slice || isTextUnmarshaler(t) || unmarshalsJSON(t)
}

// tryTextUnmarshaler attempts to use TextUnmarshaler interface if implemented
// Returns (handled, error) where handled indicates if TextUnmarshaler was used
func tryTextUnmarshaler(field reflect.Value, value any) (bool, error) {
	// A field of interface type names no concrete type to unmarshal into, so
	// it is left to setFieldByKind to refuse, rather than calling a method on
	// a nil interface.
	if field.Kind() == reflect.Interface {
		return false, nil
	}
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
		if field.Type() == durationType {
			return setDuration(field, value)
		}
		return setInt(field, value)

	case reflect.Float32, reflect.Float64:
		return setFloat(field, value)

	case reflect.Bool:
		return setBool(field, value)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return setUint(field, value)

	case reflect.Slice:
		return setSlice(field, value)

	case reflect.Map:
		return setMap(field, value)

	case reflect.Interface:
		// An empty interface, such as any, takes the value as decoded, as
		// encoding/json does; numbers stay json.Number, as with UseNumber,
		// so a large integer keeps its precision. An interface with methods
		// names no concrete type to fill.
		if field.NumMethod() == 0 {
			field.Set(reflect.ValueOf(value))
			return nil
		}
		return fmt.Errorf("unsupported type: %s", field.Type())

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
	case []any, map[string]any:
		// A JSON array or object has no single text to take, and formatting
		// one with %v would bind Go syntax such as "[a b]".
		return "", fmt.Errorf("cannot convert a JSON %s to string", jsonCompositeName(v))
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// jsonCompositeName names a decoded JSON array or object for an error message.
func jsonCompositeName(v any) string {
	if _, ok := v.([]any); ok {
		return "array"
	}
	return "object"
}

// durationType is time.Duration, which binds from text such as "5s".
var durationType = reflect.TypeFor[time.Duration]()

// setDuration sets a time.Duration. Text, from any source including a JSON
// string, is parsed with time.ParseDuration, so "5s", "1m30s" and "250ms"
// bind as written. A JSON number is a count of nanoseconds, as encoding/json
// treats it, so a client already sending numbers keeps working.
func setDuration(field reflect.Value, value any) error {
	text, ok := value.(string)
	if !ok {
		return setInt(field, value)
	}
	d, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	field.SetInt(int64(d))
	return nil
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

// setIntFromFloat writes a float to an integer field, refusing one outside
// int64's range before converting: Go's conversion of an out-of-range float is
// implementation-defined, and would otherwise saturate or wrap unreported.
func setIntFromFloat(field reflect.Value, f float64) error {
	if math.IsNaN(f) || f < math.MinInt64 || f >= math.MaxInt64 {
		return fmt.Errorf("%v overflows %s", f, field.Type())
	}
	return setIntChecked(field, int64(f))
}

// setUintFromFloat writes a float to an unsigned field, refusing a negative
// one or one outside uint64's range before converting.
func setUintFromFloat(field reflect.Value, f float64) error {
	if f < 0 {
		return fmt.Errorf("cannot convert negative float to uint")
	}
	if math.IsNaN(f) || f >= math.MaxUint64 {
		return fmt.Errorf("%v overflows %s", f, field.Type())
	}
	return setUintChecked(field, uint64(f))
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
		return setIntFromFloat(field, float64(v))
	case float64:
		return setIntFromFloat(field, v)
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
		return setIntFromFloat(field, f)
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
		return setUintFromFloat(field, v)
	case json.Number:
		if u, err := strconv.ParseUint(v.String(), 10, 64); err == nil {
			return setUintChecked(field, u)
		}
		f, err := v.Float64()
		if err != nil {
			return err
		}
		return setUintFromFloat(field, f)
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

	// A single value, such as a form field sent once or a JSON scalar, binds
	// as a one-element slice. An object does not: it is not a list of one.
	if _, isObject := value.(map[string]any); !isObject {
		return setSlice(field, []any{value})
	}

	return fmt.Errorf("cannot convert %T to slice", value)
}

// setMap fills a map from a JSON object. Each key is converted to the map's
// key type, which may be a string, an integer or a TextUnmarshaler, and each
// value as a field of the element type would be. The map is replaced rather
// than merged into, as a slice is. Every entry is attempted, and the failures
// are returned as BindErrors named by key: Meta["a"] and meta[a], the name a
// query or form spells it with.
func setMap(field reflect.Value, value any) error {
	obj, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("cannot convert %T to map", value)
	}

	typ := field.Type()
	m := reflect.MakeMapWithSize(typ, len(obj))
	var errs BindErrors
	for k, v := range obj {
		key := reflect.New(typ.Key()).Elem()
		field, name := "["+strconv.Quote(k)+"]", "["+k+"]"
		if err := setField(key, k); err != nil {
			errs = append(errs, newBindError(field, "", name, fmt.Errorf("invalid key: %w", err)))
			continue
		}
		elem := reflect.New(typ.Elem()).Elem()
		if err := bindFieldValue(elem, v); err != nil {
			var inner BindErrors
			if errors.As(err, &inner) {
				errs = append(errs, nestFailures(field, name, "", inner)...)
			} else {
				errs = append(errs, newBindError(field, "", name, err))
			}
			continue
		}
		m.SetMapIndex(key, elem)
	}
	if len(errs) > 0 {
		// Map order is random; report in key order so the result is stable.
		slices.SortFunc(errs, func(a, b *BindError) int { return strings.Compare(a.Name, b.Name) })
		return errs
	}
	field.Set(m)
	return nil
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
