// Copyright 2025 The binder Authors.

package binder

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"reflect"
	"strconv"
	"sync"
)

// decodeOptions is resolved once rather than per call, since building an
// Options value allocates.
var decodeOptions = jsontext.AllowDuplicateNames(true)

// pooledDecoder is a decoder kept for reuse together with the buffer it reads
// from. A new decoder costs several allocations: its own state, a reader and
// a read buffer. Reading from a bytes.Buffer, jsontext parses the bytes in
// place rather than copying them into a buffer of its own, so a pooled
// decoder holds no copy of an earlier request's body.
type pooledDecoder struct {
	buf bytes.Buffer
	dec jsonDecoder
}

// jsonDecoder is a jsontext.Decoder carrying one bind's failure budget: once
// maxFailures failures are recorded, later ones are dropped without being
// built, so a body of many bad values costs no more than one of a few.
type jsonDecoder struct {
	*jsontext.Decoder
	failures int
}

var decoderPool = sync.Pool{
	New: func() any {
		p := new(pooledDecoder)
		p.dec.Decoder = jsontext.NewDecoder(&p.buf, decodeOptions)
		return p
	},
}

// getDecoder returns a decoder reading data. Release it with putDecoder once
// nothing read from it is still in use.
func getDecoder(data []byte) *pooledDecoder {
	p := decoderPool.Get().(*pooledDecoder)
	p.buf = *bytes.NewBuffer(data)
	p.dec.Reset(&p.buf, decodeOptions)
	p.dec.failures = 0
	return p
}

// putDecoder returns a decoder to the pool, first dropping its reference to
// the body so that a pooled decoder never keeps a request's data alive.
func putDecoder(p *pooledDecoder) {
	p.buf = bytes.Buffer{}
	p.dec.Reset(&p.buf, decodeOptions)
	decoderPool.Put(p)
}

// decodeJSONValue reads one value, producing the types the set* helpers
// expect: string, bool, json.Number, []any, map[string]any
// and nil.
func decodeJSONValue(dec *jsonDecoder) (any, error) {
	switch dec.PeekKind() {
	case '{':
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		object := make(map[string]any)
		for dec.PeekKind() == '"' {
			nameTok, err := dec.ReadToken()
			if err != nil {
				return nil, err
			}
			name := nameTok.String()

			value, err := decodeJSONValue(dec)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		_, err := dec.ReadToken()
		return object, err

	case '[':
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		var array []any
		for dec.PeekKind() != ']' {
			value, err := decodeJSONValue(dec)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		_, err := dec.ReadToken()
		return array, err

	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		return tok.String(), nil

	case '0':
		// Kept as text so that an integer beyond float64's exact range binds
		// to the value that was sent.
		raw, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		return json.Number(raw.String()), nil

	case 't', 'f':
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		return tok.Bool(), nil

	case 'n':
		_, err := dec.ReadToken()
		return nil, err

	default:
		// PeekKind reports an invalid kind when the input is malformed.
		// Reading surfaces jsontext's own error, which names the offending
		// byte and where it appeared.
		if _, err := dec.ReadValue(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unexpected %s in JSON body", jsonKindName(dec.PeekKind()))
	}
}

// jsonKindName names a token kind for an error message.
func jsonKindName(k jsontext.Kind) string {
	switch k {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case '0':
		return "number"
	case 't', 'f':
		return "bool"
	case 'n':
		return "null"
	default:
		return "value"
	}
}

// jsonBodyInto fills a struct's body-sourced fields straight from the request
// body, without first decoding it into a map.
//
// Going through map[string]any costs an allocation per member for the
// interface value, plus the map itself, before any conversion happens. Walking
// the tokens once and writing into the destination as each member is reached
// avoids both, and lets a member no field binds be skipped without decoding it
// at all.
//
// It marks in bound, which the caller provides, the fields it filled, so the
// caller can apply required to the ones it did not, and reports the names of
// members nothing binds. A member that
// cannot be converted is recorded in errs and the walk continues; only a
// failure to read the JSON itself is returned.
func jsonBodyInto(data []byte, info *typeInfo, val reflect.Value, wantUnknown bool, bound []bool, errs *fieldErrs) (unknown []string, err error) {
	pooled := getDecoder(data)
	defer putDecoder(pooled)
	dec := &pooled.dec

	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	switch tok.Kind() {
	case 'n':
		// A null body carries no members.
		return nil, endOfBody(dec)
	case '{':
	default:
		return nil, fmt.Errorf("cannot bind a JSON %s body: want an object", jsonKindName(tok.Kind()))
	}

	var seenUnknown map[string]struct{}
	for dec.PeekKind() == '"' {
		// The name is looked up from its raw bytes, since a map lookup keyed
		// by string(b) does not allocate, where making it a string first
		// would, for every member. The bytes are voided by the next call on
		// the decoder, so an unknown name is copied before moving on.
		raw, err := dec.ReadValue()
		if err != nil {
			return nil, err
		}
		name, err := memberName(raw)
		if err != nil {
			return nil, err
		}

		index, isBound := info.bodyFields[string(name)]
		if !isBound {
			// A set, not a scan of unknown, keeps a body of many distinct
			// unknown members linear rather than quadratic.
			// Only as many as are reported are kept.
			if wantUnknown && len(unknown) < maxFailures {
				if _, dup := seenUnknown[string(name)]; !dup {
					if seenUnknown == nil {
						seenUnknown = make(map[string]struct{})
					}
					seenUnknown[string(name)] = struct{}{}
					unknown = append(unknown, string(name))
				}
			}
			if err := dec.SkipValue(); err != nil {
				return nil, err
			}
			continue
		}

		// A conversion failure has already consumed the member's value, so
		// the walk can carry on to the next one. The field counts as bound
		// either way: it was present, so required must not report it again.
		// A pointer, not a copy: fieldInfo carries a reflect.StructField and
		// is copied for every member otherwise.
		fi := &info.fields[index]

		// Once the budget is spent the bind has failed: later members are
		// only checked for being well-formed, and replace no failure.
		if dec.exhausted() {
			if err := dec.SkipValue(); err != nil {
				return nil, err
			}
			bound[index] = true
			continue
		}

		// A member sent twice binds its last occurrence, unless an earlier
		// one failed: as in encoding/json, that failure stands, and later
		// occurrences are only checked for being well-formed.
		if errs.failed(index) {
			if err := dec.SkipValue(); err != nil {
				return nil, err
			}
			continue
		}

		// A null sets nothing, so a promoted field's embedded pointer is not
		// allocated for one.
		if len(fi.Index) > 1 && dec.PeekKind() == 'n' {
			if err := dec.SkipValue(); err != nil {
				return nil, err
			}
			bound[index] = true
			continue
		}

		// A promoted omitempty field is decoded before its embedded pointer
		// is reached, so an empty value allocates nothing, as in a form body.

		if len(fi.Index) > 1 && fi.OmitEmpty {
			err := decodeUnlessEmpty(dec, fi.Plan, func() reflect.Value { return fieldAt(val, fi.Index) })
			var failed conversionFailure
			if errors.As(err, &failed) {
				f := fieldFailures(*fi, failed.err)
				dec.charge(f)
				errs.set(info, index, f)
			} else if err != nil {
				return nil, err
			}
			bound[index] = true
			continue
		}

		if err := decodeJSONInto(dec, fieldAt(val, fi.Index), fi); err != nil {
			var failed conversionFailure
			if !errors.As(err, &failed) {
				return nil, err
			}
			f := fieldFailures(*fi, failed.err)
			dec.charge(f)
			errs.set(info, index, f)
		}
		bound[index] = true
	}

	if _, err := dec.ReadToken(); err != nil { // the closing brace
		return nil, err
	}
	return unknown, endOfBody(dec)
}

// endOfBody reports anything after the body's one JSON value, such as a
// second object, which would otherwise be ignored. Trailing whitespace is
// allowed.
func endOfBody(dec *jsonDecoder) error {
	if dec.PeekKind() != 0 {
		return errors.New("unexpected data after top-level value")
	}
	if _, err := dec.ReadToken(); err != io.EOF {
		return fmt.Errorf("unexpected data after top-level value: %w", err)
	}
	return nil
}

// memberName returns an object member's name from its raw, quoted form. A name
// without escapes is the bytes between the quotes, with nothing to allocate;
// only one written with escapes, such as "\u0061ge", is decoded.
func memberName(raw jsontext.Value) ([]byte, error) {
	if bytes.IndexByte(raw, '\\') < 0 {
		return raw[1 : len(raw)-1], nil
	}
	return jsontext.AppendUnquote(nil, raw)
}

// decodeJSONInto writes one JSON value into a struct field. Where the field is
// a predeclared type and the token already matches it, the value is set
// directly; everything else falls back to decoding a value and converting it,
// so coercions such as a JSON string into an integer keep working.
func decodeJSONInto(dec *jsonDecoder, field reflect.Value, fi *fieldInfo) error {
	kind := dec.PeekKind()

	// omitempty on anything but a predeclared type, whose fast path judges
	// emptiness itself, is decided as the value is read.
	if fi.OmitEmpty && (fi.Fast == fastNone || kind == '[' || kind == '{') {
		return decodeUnlessEmpty(dec, fi.Plan, func() reflect.Value { return field })
	}

	// A type that decodes itself from JSON is handed the member exactly as
	// sent. A null sets nothing, as for any field, and a string goes to
	// UnmarshalText when the type has it, as setField does.
	if fi.JSON && kind != 'n' && (kind != '"' || !fi.Plan.text) {
		raw, err := dec.ReadValue()
		if err != nil {
			return err
		}
		// The decoder reuses raw's memory, and a type may keep what it is given.
		return conversionError(unmarshalJSONRaw(field, bytes.Clone(raw)))
	}

	// An array, object or nested struct is decoded straight into its Go
	// type rather than through []any and map[string]any, which cost several
	// times the body's size.
	if d := fi.Plan.direct; d != 0 && d == kind {
		return decodeValueInto(dec, field)
	}

	if handled, err := decodeFast(dec, field, fi.Fast, kind, fi.OmitEmpty); handled {
		return err
	}
	if mismatch, err := skipMismatch(dec, field.Type(), kind); mismatch {
		return err
	}

	// The token does not match the destination, or the destination is not a
	// predeclared type: decode the value and convert it as the other sources
	// do, so coercions such as a JSON string into an integer keep working.
	value, err := decodeJSONValue(dec)
	if err != nil {
		return err
	}
	if value == nil {
		return nil
	}
	if fi.OmitEmpty && isEmptyValue(value) {
		return nil
	}
	return conversionError(setJSONValue(field, value))
}

// decodePlan is what decoding needs to know about a type, resolved once: the
// checks behind it go through reflect's Implements, which is too slow to ask
// for every element of an array.
type decodePlan struct {
	typ    reflect.Type
	direct jsontext.Kind // '[' or '{' when decoded token by token, through any pointers
	fast   fastKind      // set straight from a matching token
	json   bool          // decodes itself from JSON
	text   bool          // unmarshals itself from text
}

var decodePlans sync.Map // reflect.Type -> *decodePlan

// fileHeaderStruct is multipart.FileHeader, which only a multipart form fills.
var fileHeaderStruct = reflect.TypeFor[multipart.FileHeader]()

func planFor(t reflect.Type) *decodePlan {
	if p, ok := decodePlans.Load(t); ok {
		return p.(*decodePlan)
	}
	p := &decodePlan{typ: t, fast: fastKindOf(t), json: unmarshalsJSON(t), text: isTextUnmarshaler(t)}
	base := t
	for i := 0; base.Kind() == reflect.Pointer && i < maxPointerDepth; i++ {
		base = base.Elem()
	}
	// A file upload arrives only in a multipart form; a JSON object must not
	// fill one and pass for a file.
	if !isTextUnmarshaler(base) && !unmarshalsJSON(base) && base != fileHeaderStruct {
		switch base.Kind() {
		case reflect.Slice:
			p.direct = '['
		case reflect.Map, reflect.Struct:
			p.direct = '{'
		}
	}
	decodePlans.Store(t, p)
	return p
}

// decodeValueInto reads one JSON value into v, a slice, map or struct field
// or an element of one, walking the tokens rather than decoding into
// interface values first. It follows the same rules as the general path: a
// null sets nothing, a slice or map is replaced, a nested struct binds its
// body and json tags with required and omitempty, and every failure is
// reported, named by path, in a conversionFailure. Any other error is the
// JSON itself.
func decodeValueInto(dec *jsonDecoder, v reflect.Value) error {
	return decodeWithPlan(dec, v, planFor(v.Type()))
}

// decodeWithPlan is decodeValueInto with v's plan already in hand, so that an
// array or map looks its element type's plan up once rather than per element.
func decodeWithPlan(dec *jsonDecoder, v reflect.Value, p *decodePlan) error {
	// Once the budget is spent the bind has failed, so what remains is only
	// checked for being well-formed, not decoded.
	if dec.exhausted() {
		return dec.SkipValue()
	}
	kind := dec.PeekKind()
	if kind == 'n' {
		_, err := dec.ReadToken()
		return err
	}
	if p.direct == 0 || p.direct != kind {
		return decodeScalarInto(dec, v, p, kind)
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		return decodeValueInto(dec, v.Elem())
	}

	switch v.Kind() {
	case reflect.Slice:
		return decodeArrayInto(dec, v)
	case reflect.Map:
		return decodeObjectIntoMap(dec, v)
	default:
		return decodeObjectIntoStruct(dec, v)
	}
}

// decodeScalarInto reads one value into a field that is not decoded token by
// token: a predeclared type from a matching token directly, anything else
// through the general conversion, so coercions and custom types work as they
// do at the top level.
func decodeScalarInto(dec *jsonDecoder, v reflect.Value, p *decodePlan, kind jsontext.Kind) error {
	if p.json && (kind != '"' || !p.text) {
		raw, err := dec.ReadValue()
		if err != nil {
			return err
		}
		return conversionError(unmarshalJSONRaw(v, bytes.Clone(raw)))
	}
	if handled, err := decodeFast(dec, v, p.fast, kind, false); handled {
		return err
	}
	if mismatch, err := skipMismatch(dec, v.Type(), kind); mismatch {
		return err
	}
	value, err := decodeJSONValue(dec)
	if err != nil || value == nil {
		return err
	}
	return conversionError(setJSONValue(v, value))
}

// decodeUnlessEmpty reads one value for an omitempty field and decodes it into
// the field target returns, unless it is empty: "", 0, false, null, {} or [].
// Every byte is read once, but for a value an empty interface takes, which is
// parsed again from its raw form. An array or object is judged by peeking past
// its opening token for the closing one, and otherwise decoded from there, so
// a deep value of nested omitempty fields costs no more than a flat one. target
// is called only for a value that will be set, so an empty one allocates no
// embedded pointer on the way.
func decodeUnlessEmpty(dec *jsonDecoder, p *decodePlan, target func() reflect.Value) error {
	if dec.exhausted() {
		return dec.SkipValue()
	}
	kind := dec.PeekKind()
	switch {
	case kind == 'n':
		_, err := dec.ReadToken()
		return err

	case p.json && (kind != '"' || !p.text):
		// A type that decodes itself is handed the value exactly as sent.
		raw, err := dec.ReadValue()
		if err != nil || rawJSONEmpty(raw) {
			return err
		}
		return conversionError(unmarshalJSONRaw(target(), bytes.Clone(raw)))

	case p.direct != 0 && p.direct == kind:
		if _, err := dec.ReadToken(); err != nil {
			return err
		}
		closing := jsontext.Kind(']')
		if kind == '{' {
			closing = '}'
		}
		if dec.PeekKind() == closing {
			_, err := dec.ReadToken()
			return err
		}
		v := target()
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		switch v.Kind() {
		case reflect.Slice:
			return decodeArrayRest(dec, v)
		case reflect.Map:
			return decodeObjectRestIntoMap(dec, v)
		default:
			return decodeObjectRestIntoStruct(dec, v)
		}

	case kind == '[' || kind == '{':
		// An array or object for a type that does not take one directly is
		// skipped when empty, as omitempty says, and otherwise refused,
		// unless the field is an empty interface, which takes it as decoded.
		raw, err := dec.ReadValue()
		if err != nil || rawJSONEmpty(raw) {
			return err
		}
		if !takesAnyValue(p.typ) {
			return conversionError(fmt.Errorf("cannot bind a JSON %s to %s", jsonKindName(kind), p.typ))
		}
		sub := &jsonDecoder{Decoder: jsontext.NewDecoder(bytes.NewReader(bytes.Clone(raw)), decodeOptions)}
		value, err := decodeJSONValue(sub)
		if err != nil {
			return err
		}
		return conversionError(setJSONValue(target(), value))

	default:
		value, err := decodeJSONValue(dec)
		if err != nil || isEmptyValue(value) {
			return err
		}
		return conversionError(setJSONValue(target(), value))
	}
}

// rawJSONEmpty reports whether a raw JSON value is empty in omitempty's sense,
// as isEmptyValue judges a decoded one.
func rawJSONEmpty(raw jsontext.Value) bool {
	switch raw.Kind() {
	case 'n', 'f':
		return true
	case '"':
		return len(raw) == 2
	case '0':
		return isEmptyValue(json.Number(raw.String()))
	case '{', '[':
		inner := bytes.TrimSpace(raw[1 : len(raw)-1])
		return len(inner) == 0
	default:
		return false
	}
}

// skipMismatch skips an array or object bound for a type that cannot take
// one, reporting the mismatch without decoding the value: only an empty
// interface, such as any, takes an array or object that is not decoded
// token by token. Decoding it first would cost memory in proportion
// to a value that is refused anyway.
func skipMismatch(dec *jsonDecoder, t reflect.Type, kind jsontext.Kind) (bool, error) {
	if kind != '[' && kind != '{' {
		return false, nil
	}
	if takesAnyValue(t) {
		return false, nil
	}
	base := t
	for i := 0; base.Kind() == reflect.Pointer && i < maxPointerDepth; i++ {
		base = base.Elem()
	}
	if err := dec.SkipValue(); err != nil {
		return true, err
	}
	if base.Kind() == reflect.Array {
		return true, conversionError(errors.New("arrays are not supported, use slices instead"))
	}
	return true, conversionError(fmt.Errorf("cannot bind a JSON %s to %s", jsonKindName(kind), t))
}

// takesAnyValue reports whether t, through any pointers, is an empty
// interface such as any, which takes whatever JSON value it is given.
func takesAnyValue(t reflect.Type) bool {
	for i := 0; t.Kind() == reflect.Pointer && i < maxPointerDepth; i++ {
		t = t.Elem()
	}
	return t.Kind() == reflect.Interface && t.NumMethod() == 0
}

// setJSONValue sets a value decoded from a JSON body. It is setField with
// one rule JSON alone has: a string into a []byte is base64, as encoding/json
// encodes one.
func setJSONValue(v reflect.Value, value any) error {
	if text, ok := value.(string); ok {
		// A []byte behind pointers, such as *[]byte, is still a []byte.
		target := v
		for i := 0; target.Kind() == reflect.Pointer && i < maxPointerDepth; i++ {
			if target.IsNil() {
				target.Set(reflect.New(target.Type().Elem()))
			}
			target = target.Elem()
		}
		if isByteSlice(target.Type()) {
			b, err := base64.StdEncoding.DecodeString(text)
			if err != nil {
				return fmt.Errorf("invalid base64: %w", err)
			}
			target.SetBytes(b)
			return nil
		}
		// A single string into a list is a list of one, its element set by
		// these same rules, so that a [][]byte element is base64 too.
		if target.Kind() == reflect.Slice && !takesOneValue(target.Type()) && wrapsFinitely(target.Type()) {
			s := reflect.MakeSlice(target.Type(), 1, 1)
			if err := setJSONValue(s.Index(0), value); err != nil {
				var inner BindErrors
				if errors.As(err, &inner) {
					return BindErrors(nestIndexFailures("[0]", "[0]", inner))
				}
				return BindErrors{newIndexBindError("[0]", "[0]", err)}
			}
			target.Set(s)
			return nil
		}
	}
	return bindFieldValue(v, value)
}

// isByteSlice reports whether t is a slice of bytes that does not decode
// itself, as []byte is.
func isByteSlice(t reflect.Type) bool {
	return t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 &&
		!isTextUnmarshaler(t) && !unmarshalsJSON(t) && !isTextUnmarshaler(t.Elem()) && !unmarshalsJSON(t.Elem())
}

// collectFailure adds a child's conversion failure to errs under the given
// path, and returns any other error, which ends the walk.
func collectFailure(dec *jsonDecoder, errs *BindErrors, err error, part pathPart) error {
	if err == nil {
		return nil
	}
	var failed conversionFailure
	if !errors.As(err, &failed) {
		return err
	}
	var inner BindErrors
	if errors.As(failed.err, &inner) {
		// Failures decoded token by token were counted where they arose;
		// ones a conversion grouped, such as a slice's from one bad value,
		// are counted here, and any past the budget dropped.
		kept := inner[:0]
		for _, e := range inner {
			if e.counted || dec.budget() {
				e.counted = true
				kept = append(kept, e)
			}
		}
		// The child's list is passed up rather than copied when it is the
		// only one, so a deep value's failures are not copied level by level.
		if len(*errs) == 0 {
			*errs = nestFailuresAs(part, "", kept)
		} else {
			*errs = append(*errs, nestFailuresAs(part, "", kept)...)
		}
	} else if dec.budget() {
		e := newBindError(part.field, "", part.name, failed.err)
		e.leafIndex = part.index
		e.counted = true
		*errs = append(*errs, e)
	}
	return nil
}

// charge counts failures not yet charged, such as a top-level field's, which
// are recorded whatever the budget: there are only as many as fields.
func (d *jsonDecoder) charge(errs []*BindError) {
	for _, e := range errs {
		if !e.counted {
			e.counted = true
			d.failures++
		}
	}
}

// exhausted reports whether the failure budget is spent.
func (d *jsonDecoder) exhausted() bool { return d.failures >= maxFailures }

// budget reports whether another failure may be recorded, counting it if so.
func (d *jsonDecoder) budget() bool {
	if d.failures >= maxFailures {
		return false
	}
	d.failures++
	return true
}

// decodeArrayInto fills a slice from a JSON array, replacing what it held. A
// null element leaves its zero value, so a nil pointer stays nil.
func decodeArrayInto(dec *jsonDecoder, v reflect.Value) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	return decodeArrayRest(dec, v)
}

// decodeArrayRest is decodeArrayInto once the opening bracket has been read.
func decodeArrayRest(dec *jsonDecoder, v reflect.Value) error {
	// An empty array needs no buffer to grow into.
	if dec.PeekKind() == ']' {
		if _, err := dec.ReadToken(); err != nil {
			return err
		}
		v.Set(reflect.MakeSlice(v.Type(), 0, 0))
		return nil
	}

	// The slice is grown in place, doubling as reflect.Append would, but
	// without Append's allocation on every call: an addressable slice value
	// is extended with SetLen and replaced only when it is full.
	elemPlan := planFor(v.Type().Elem())
	s := reflect.New(v.Type()).Elem()
	s.Set(reflect.MakeSlice(v.Type(), 0, 1))
	var errs BindErrors
	for i := 0; dec.PeekKind() != ']'; i++ {
		if dec.PeekKind() == 0 {
			_, err := dec.ReadToken()
			return err
		}
		if dec.exhausted() {
			if err := dec.SkipValue(); err != nil {
				return err
			}
			continue
		}
		if i == s.Cap() {
			grown := reflect.MakeSlice(v.Type(), i, max(2*i, 4))
			reflect.Copy(grown, s)
			s.Set(grown)
		}
		s.SetLen(i + 1)
		if err := decodeWithPlan(dec, s.Index(i), elemPlan); err != nil {
			// The element's name is built only when it has failed.
			index := "[" + strconv.Itoa(i) + "]"
			if err := collectFailure(dec, &errs, err, pathPart{field: index, name: index, index: true}); err != nil {
				return err
			}
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	if len(errs) > 0 {
		return conversionFailure{errs}
	}
	// Elements skipped once the budget was spent were never decoded, so the
	// slice is not set from those that were; the bind has failed anyway.
	if !dec.exhausted() {
		v.Set(s)
	}
	return nil
}

// decodeObjectIntoMap fills a map from a JSON object, replacing what it held.
// Keys convert as setMap converts them, a null value gives its key the zero
// value, as in encoding/json, and a failure is named by key.
func decodeObjectIntoMap(dec *jsonDecoder, v reflect.Value) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	return decodeObjectRestIntoMap(dec, v)
}

// decodeObjectRestIntoMap is decodeObjectIntoMap once the opening brace has
// been read. A key sent twice binds its last occurrence, unless an earlier
// one failed: that failure stands.
func decodeObjectRestIntoMap(dec *jsonDecoder, v reflect.Value) error {
	typ := v.Type()
	elemPlan := planFor(typ.Elem())
	m := reflect.MakeMap(typ)
	var failed map[any]keyFailures
	// Keys that failed, by their text as sent: a key whose converted value
	// never equals itself, such as NaN or a pointer, is still recognised
	// when it is sent again.
	var failedText map[string]bool
	// One key and one element are reused for every entry: SetMapIndex
	// copies them into the map.
	key := reflect.New(typ.Key()).Elem()
	elem := reflect.New(typ.Elem()).Elem()
	for dec.PeekKind() == '"' {
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		k := tok.String()
		if dec.exhausted() {
			if err := dec.SkipValue(); err != nil {
				return err
			}
			continue
		}
		if failedText[k] {
			if err := dec.SkipValue(); err != nil {
				return err
			}
			continue
		}
		key.SetZero()
		if err := convertMapKey(key, k); err != nil {
			if _, dup := failed[k]; !dup && dec.budget() {
				e := newIndexBindError(mapKeyField(k), mapKeyName(k), fmt.Errorf("invalid key: %w", err))
				e.counted = true
				failed = addKeyFailures(failed, k, k, BindErrors{e})
			}
			if err := dec.SkipValue(); err != nil {
				return err
			}
			continue
		}
		// Failures are kept by the key's converted value, so "01" and "1"
		// into an integer key are one entry. A key sent again after it
		// failed keeps that failure, and its value is only checked for
		// being well-formed.
		var id any
		if failed != nil {
			id = key.Interface()
			if _, dup := failed[id]; dup {
				if err := dec.SkipValue(); err != nil {
					return err
				}
				continue
			}
		}
		elem.SetZero()
		var errs BindErrors
		if err := decodeWithPlan(dec, elem, elemPlan); err != nil {
			if err := collectFailure(dec, &errs, err, pathPart{field: mapKeyField(k), name: mapKeyName(k), index: true}); err != nil {
				return err
			}
		}
		if len(errs) > 0 {
			if id == nil {
				id = key.Interface()
			}
			failed = addKeyFailures(failed, id, k, errs)
			if failedText == nil {
				failedText = make(map[string]bool)
			}
			failedText[k] = true
			continue
		}
		// A value skipped once the budget was spent was never decoded.
		if dec.exhausted() {
			continue
		}
		m.SetMapIndex(key, elem)
	}
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	if len(failed) > 0 {
		return conversionFailure{keyOrderedFailures(typ.Key(), failed)}
	}
	if !dec.exhausted() {
		v.Set(m)
	}
	return nil
}

// decodeObjectIntoStruct fills a nested struct from a JSON object: members
// bind by body or json tag,
// with promoted fields, required and omitempty, a null sets nothing, members
// no field binds are skipped, and failures come in field order, named by path.
// A member sent twice binds its last occurrence, unless an earlier one failed;
// a struct member sent twice merges into the same struct.
func decodeObjectIntoStruct(dec *jsonDecoder, v reflect.Value) error {
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	return decodeObjectRestIntoStruct(dec, v)
}

// decodeObjectRestIntoStruct is decodeObjectIntoStruct once the opening brace
// has been read.
func decodeObjectRestIntoStruct(dec *jsonDecoder, v reflect.Value) error {
	ns := nestedStructFor(v.Type())
	fields := ns.fields

	// Which fields were sent matters only for required ones, and a failure
	// list only once something fails, so an object that binds cleanly, the
	// usual case, allocates for neither: a struct of up to 64 fields tracks
	// what was sent in a word on the stack.
	var sentBits uint64
	var sent []bool
	if len(fields) > 64 && ns.anyRequired {
		sent = make([]bool, len(fields))
	}
	markSent := func(i int) {
		if i < 64 {
			sentBits |= 1 << i
		} else if sent != nil {
			sent[i] = true
		}
	}
	wasSent := func(i int) bool {
		if i < 64 {
			return sentBits&(1<<i) != 0
		}
		return sent != nil && sent[i]
	}
	var failures []BindErrors

	for dec.PeekKind() == '"' {
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		i, ok := ns.index[tok.String()]
		if !ok {
			if err := dec.SkipValue(); err != nil {
				return err
			}
			continue
		}
		tf := fields[i]
		markSent(i)
		// A member sent twice binds its last occurrence, unless an earlier
		// one failed: that failure stands, and later occurrences are only
		// checked for being well-formed.
		if failures != nil && len(failures[i]) > 0 {
			if err := dec.SkipValue(); err != nil {
				return err
			}
			continue
		}

		if dec.PeekKind() == 'n' {
			if _, err := dec.ReadToken(); err != nil {
				return err
			}
			continue
		}
		var err2 error
		if omitsEmpty(tf.field.Type, tf.opts) {
			// An empty value must not allocate an embedded pointer on the
			// way to its field, so the field is reached only once the value
			// is known not to be empty.
			err2 = decodeUnlessEmpty(dec, ns.plans[i], func() reflect.Value { return fieldByIndex(v, tf.index) })
		} else {
			err2 = decodeWithPlan(dec, fieldByIndex(v, tf.index), ns.plans[i])
		}
		if err2 != nil {
			if failures == nil {
				failures = make([]BindErrors, len(fields))
			}
			if err := collectFailure(dec, &failures[i], err2, pathPart{field: tf.goName, name: tf.name}); err != nil {
				return err
			}
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return err
	}

	var errs BindErrors
	for i, tf := range fields {
		if ns.required[i] && !wasSent(i) && dec.budget() {
			errs = append(errs, &BindError{
				Field:   tf.goName,
				Source:  body,
				Name:    tf.name,
				Message: fmt.Sprintf("missing required field %s: no %s value named %q", tf.goName, body, tf.name),
				Err:     ErrMissingRequired,
				counted: true,
			})
		}
		if failures == nil || len(failures[i]) == 0 {
			continue
		}
		// A member's failures were recorded with its path already; the
		// source is set once the whole struct is placed under its field. A
		// lone list is taken as it is rather than copied.
		if errs == nil {
			errs = failures[i]
		} else {
			errs = append(errs, failures[i]...)
		}
	}
	if len(errs) > 0 {
		return conversionFailure{errs}
	}
	return nil
}

// nestedStruct is what decoding a nested struct from a JSON object needs,
// resolved once per type: its fields, a lookup by body key, each field's
// decoding plan, and which fields are required.
type nestedStruct struct {
	fields      []taggedField
	index       map[string]int
	plans       []*decodePlan
	required    []bool
	anyRequired bool
}

var nestedStructCache sync.Map // reflect.Type -> *nestedStruct

func nestedStructFor(typ reflect.Type) *nestedStruct {
	if cached, ok := nestedStructCache.Load(typ); ok {
		return cached.(*nestedStruct)
	}
	fields := nestedFieldsFor(typ)
	ns := &nestedStruct{
		fields:   fields,
		index:    make(map[string]int, len(fields)),
		plans:    make([]*decodePlan, len(fields)),
		required: make([]bool, len(fields)),
	}
	for i, tf := range fields {
		ns.index[tf.name] = i
		ns.plans[i] = planFor(tf.field.Type)
		if hasOption(tf.opts, optRequired) {
			ns.required[i], ns.anyRequired = true, true
		}
	}
	nestedStructCache.Store(typ, ns)
	return ns
}

// decodeFast sets a predeclared field straight from a token of the matching
// kind, reporting whether it did. omitEmpty leaves the field alone for an
// empty value.
func decodeFast(dec *jsonDecoder, field reflect.Value, fast fastKind, kind jsontext.Kind, omitEmpty bool) (bool, error) {
	switch fast {
	case fastString:
		if kind == '"' {
			tok, err := dec.ReadToken()
			if err != nil {
				return true, err
			}
			text := tok.String()
			if omitEmpty && text == "" {
				return true, nil
			}
			field.SetString(text)
			return true, nil
		}

	case fastBool:
		if kind == 't' || kind == 'f' {
			tok, err := dec.ReadToken()
			if err != nil {
				return true, err
			}
			value := tok.Bool()
			if omitEmpty && !value {
				return true, nil
			}
			field.SetBool(value)
			return true, nil
		}

	case fastInt, fastUint, fastFloat:
		if kind == '0' {
			return true, decodeNumberInto(dec, field, fast, omitEmpty)
		}
	}
	return false, nil
}

// decodeNumberInto writes a numeric token into a predeclared numeric field.
// A literal an exact parse rejects, such as 1e3 for an integer, is handed to
// the general conversion, which accepts it as earlier releases did.
func decodeNumberInto(dec *jsonDecoder, field reflect.Value, fast fastKind, omitEmpty bool) error {
	raw, err := dec.ReadValue()
	if err != nil {
		return err
	}

	switch fast {
	case fastInt:
		number, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return setFieldFromNumber(field, raw, omitEmpty)
		}
		if omitEmpty && number == 0 {
			return nil
		}
		return conversionError(setIntChecked(field, number))

	case fastUint:
		number, err := strconv.ParseUint(string(raw), 10, 64)
		if err != nil {
			return setFieldFromNumber(field, raw, omitEmpty)
		}
		if omitEmpty && number == 0 {
			return nil
		}
		return conversionError(setUintChecked(field, number))

	default: // fastFloat
		number, err := strconv.ParseFloat(string(raw), 64)
		if err != nil {
			return setFieldFromNumber(field, raw, omitEmpty)
		}
		if omitEmpty && number == 0 {
			return nil
		}
		return conversionError(setFloatChecked(field, number))
	}
}

// setFieldFromNumber converts a number whose literal an exact parse rejected,
// such as 1e3 into an integer field.
func setFieldFromNumber(field reflect.Value, raw jsontext.Value, omitEmpty bool) error {
	number := json.Number(raw.String())
	if omitEmpty && isEmptyValue(number) {
		return nil
	}
	return conversionError(setField(field, number))
}

// conversionFailure marks a failure to convert a decoded value as concerning
// one field, so that it is reported as a BindError naming it rather than as a
// malformed body. A syntax error from the decoder is a different thing and
// stays as it is.
type conversionFailure struct{ err error }

func (c conversionFailure) Error() string { return c.err.Error() }

func conversionError(err error) error {
	if err == nil {
		return nil
	}
	return conversionFailure{err}
}
