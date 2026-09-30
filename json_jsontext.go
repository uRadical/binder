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
	"slices"
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
// It marks in bound, which the caller provides, the fields that were sent, so
// the caller can apply required to the ones that were not, and reports the
// names of members nothing binds. A member that cannot be converted is
// recorded in errs and the walk continues; only a failure to read the JSON
// itself is returned.
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

	var want *[]string
	if wantUnknown {
		want = &unknown
	}
	sent, failures, err := decodeFields(dec, &info.body, val, want)
	if err != nil {
		return nil, err
	}
	for i := range info.fields {
		// A member that failed still counts as bound: it was present, so
		// required must not report it again.
		if sent.has(i) {
			bound[i] = true
		}
		if failures != nil && len(failures[i]) > 0 {
			for _, e := range failures[i] {
				e.Source = info.fields[i].Source
			}
			errs.set(info, i, failures[i])
		}
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

// decodePlan is what decoding needs to know about a type, resolved once: the
// checks behind it go through reflect's Implements, which is too slow to ask
// for every element of an array.
type decodePlan struct {
	typ      reflect.Type
	direct   jsontext.Kind // '[' or '{' when decoded token by token, through any pointers
	fast     fastKind      // set straight from a matching token
	json     bool          // decodes itself from JSON
	text     bool          // unmarshals itself from text
	anyValue bool          // an empty interface, which takes any value
}

var decodePlans sync.Map // reflect.Type -> *decodePlan

// fileHeaderStruct is multipart.FileHeader, which only a multipart form fills.
var fileHeaderStruct = reflect.TypeFor[multipart.FileHeader]()

func planFor(t reflect.Type) *decodePlan {
	if p, ok := decodePlans.Load(t); ok {
		return p.(*decodePlan)
	}
	p := &decodePlan{typ: t, fast: fastKindOf(t), json: unmarshalsJSON(t), text: isTextUnmarshaler(t), anyValue: takesAnyValue(t)}
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

// decodeWithPlan reads one JSON value into v, a field or an element, whose
// plan p is. A null sets nothing, a slice or map is replaced, a nested struct
// binds its body and json tags with required and omitempty, and every failure
// is reported, named by path, in a conversionFailure. Any other error is the
// JSON itself. A pointer that was nil is left nil if the value fails.
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
	target := func() reflect.Value { return v }
	if isNilPointer(v) {
		return unsetOnFailure(v, decodeInto(dec, p, kind, false, target))
	}
	return decodeInto(dec, p, kind, false, target)
}

// decodeInto reads one value of the given kind, not null, into the field
// target returns, following p:
//
//   - a type that decodes itself is handed the value exactly as sent;
//   - an array or object the type takes is decoded token by token, rather
//     than through []any and map[string]any, which cost several times the
//     body's size;
//   - any other array or object is refused unread, unless the field is an
//     empty interface, which takes it as decoded;
//   - a predeclared type is set straight from a matching token;
//   - anything else is decoded and converted as the other sources convert
//     text, so coercions such as a JSON string into an integer work.
//
// With omitEmpty an empty value, "", 0, false, {} or [], sets nothing, and
// target is called only for a value that will be set.
func decodeInto(dec *jsonDecoder, p *decodePlan, kind jsontext.Kind, omitEmpty bool, target func() reflect.Value) error {
	switch {
	case p.json && (kind != '"' || !p.text):
		raw, err := dec.ReadValue()
		if err != nil || omitEmpty && rawJSONEmpty(raw) {
			return err
		}
		// The decoder reuses raw's memory, and a type may keep what it is given.
		return dec.fail(unmarshalJSONRaw(target(), bytes.Clone(raw)))

	case p.direct != 0 && p.direct == kind:
		if _, err := dec.ReadToken(); err != nil {
			return err
		}
		// An empty array or object is judged by peeking past its opening
		// token, so a deep value of nested omitempty fields is read once.
		if omitEmpty && (dec.PeekKind() == ']' || dec.PeekKind() == '}') {
			_, err := dec.ReadToken()
			return err
		}
		return decodeOpened(dec, target())

	case (kind == '[' || kind == '{') && !p.anyValue:
		// Decoding a value that is refused anyway would cost memory in
		// proportion to it.
		raw, err := dec.ReadValue()
		if err != nil || omitEmpty && rawJSONEmpty(raw) {
			return err
		}
		return dec.fail(mismatchError(p.typ, kind))
	}

	if handled, err := decodeFast(dec, p.fast, kind, omitEmpty, target); handled {
		return err
	}
	value, err := decodeJSONValue(dec)
	if err != nil || value == nil || omitEmpty && isEmptyValue(value) {
		return err
	}
	return dec.fail(setJSONValue(target(), value))
}

// decodeOpened decodes an array or object whose opening token has been read
// into v, allocating any pointers on the way to the slice, map or struct.
func decodeOpened(dec *jsonDecoder, v reflect.Value) error {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Slice:
		return decodeArray(dec, v)
	case reflect.Map:
		return decodeMap(dec, v)
	default:
		return decodeStruct(dec, v)
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

// mismatchError reports an array or object for a type that cannot take one.
func mismatchError(t reflect.Type, kind jsontext.Kind) error {
	base := t
	for i := 0; base.Kind() == reflect.Pointer && i < maxPointerDepth; i++ {
		base = base.Elem()
	}
	if base.Kind() == reflect.Array {
		return errors.New("arrays are not supported, use slices instead")
	}
	return fmt.Errorf("cannot bind a JSON %s to %s", jsonKindName(kind), t)
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
				return indexFailures("[0]", "[0]", err)
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
// path, and returns any other error, which ends the walk. The failure was
// charged to the budget where it arose.
func collectFailure(errs *BindErrors, err error, part pathPart) error {
	if err == nil {
		return nil
	}
	var failed conversionFailure
	if !errors.As(err, &failed) {
		return err
	}
	var inner BindErrors
	if !errors.As(failed.err, &inner) {
		e := newBindError(part.field, "", part.name, failed.err)
		e.leafIndex = part.index
		*errs = append(*errs, e)
		return nil
	}
	// The child's list is passed up rather than copied when it is the only
	// one, so a deep value's failures are not copied level by level.
	if len(*errs) == 0 {
		*errs = nestFailuresAs(part, "", inner)
	} else {
		*errs = append(*errs, nestFailuresAs(part, "", inner)...)
	}
	return nil
}

// fail reports a value that could not be converted as a conversionFailure,
// charging it to the budget where it arises, so that it is counted once
// however far it is passed up. A group, such as a slice's elements from one
// bad value, counts each of them; there are at most maxFailures, and past the
// budget nothing more is decoded, so what it may exceed the budget by is
// bounded, and the list is cut to maxFailures once binding is done.
func (d *jsonDecoder) fail(err error) error {
	if err == nil {
		return nil
	}
	var group BindErrors
	if errors.As(err, &group) {
		d.failures += len(group)
	} else {
		d.failures++
	}
	return conversionFailure{err}
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

// decodeArray fills a slice from a JSON array whose opening bracket has been
// read, replacing what it held. A null element leaves its zero value, so a nil
// pointer stays nil.
func decodeArray(dec *jsonDecoder, v reflect.Value) error {
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
			if err := collectFailure(&errs, err, pathPart{field: index, name: index, index: true}); err != nil {
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

// decodeMap fills a map from a JSON object whose opening brace has been read,
// replacing what it held. Keys convert as setMap converts them, a null value
// gives its key the zero value, as in encoding/json, and a failure is named by
// key. A key sent twice binds its last occurrence, unless an earlier one
// failed: that failure stands.
func decodeMap(dec *jsonDecoder, v reflect.Value) error {
	typ := v.Type()
	elemPlan := planFor(typ.Elem())
	m := reflect.MakeMap(typ)
	var failed mapFailures
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
		// Past the budget, or after the key has failed, the value is only
		// checked for being well-formed.
		if dec.exhausted() || failed.repeats(k) {
			if err := dec.SkipValue(); err != nil {
				return err
			}
			continue
		}
		key.SetZero()
		if err := convertMapKey(key, k); err != nil {
			if dec.budget() {
				failed.addText(k, BindErrors{newIndexBindError(mapKeyField(k), mapKeyName(k), fmt.Errorf("invalid key: %w", err))})
			}
			if err := dec.SkipValue(); err != nil {
				return err
			}
			continue
		}
		if failed.has(key) {
			if err := dec.SkipValue(); err != nil {
				return err
			}
			continue
		}
		elem.SetZero()
		if err := decodeWithPlan(dec, elem, elemPlan); err != nil {
			// The entry's name is built only when it has failed.
			var errs BindErrors
			if err := collectFailure(&errs, err, pathPart{field: mapKeyField(k), name: mapKeyName(k), index: true}); err != nil {
				return err
			}
			failed.add(key, k, errs)
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
	if errs := failed.list(typ.Key()); errs != nil {
		return conversionFailure{errs}
	}
	if !dec.exhausted() {
		v.Set(m)
	}
	return nil
}

// decodeStruct fills a nested struct from a JSON object whose opening brace
// has been read: members bind by body or json tag, with promoted fields,
// required and omitempty, a null sets nothing, members no field binds are
// skipped, and failures come in field order, named by path. A member sent
// twice binds its last occurrence, unless an earlier one failed; a struct
// member sent twice merges into the same struct.
func decodeStruct(dec *jsonDecoder, v reflect.Value) error {
	ns := nestedStructFor(v.Type())
	sent, failures, err := decodeFields(dec, &ns.objectFields, v, nil)
	if err != nil {
		return err
	}
	var errs BindErrors
	for i, part := range ns.parts {
		if ns.required[i] && !sent.has(i) && dec.budget() {
			errs = append(errs, &BindError{
				Field:   part.field,
				Source:  body,
				Name:    part.name,
				Message: fmt.Sprintf("missing required field %s: no %s value named %q", part.field, body, part.name),
				Err:     ErrMissingRequired,
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

// objectFields is what decoding a JSON object into a struct needs, resolved
// once per type: each field by the member it binds, and per field its plan,
// whether it is omitempty, its path through any embedded structs, and how a
// failure in it is named. Its indexes are those of the fields it was built
// from: typeInfo.fields at the top level, nestedFieldsFor within, where only
// the fields a body binds have entries in index.
type objectFields struct {
	index     map[string]int
	plans     []*decodePlan
	omitEmpty []bool
	paths     [][]int
	parts     []pathPart
}

func newObjectFields(n int) objectFields {
	return objectFields{
		index:     make(map[string]int, n),
		plans:     make([]*decodePlan, n),
		omitEmpty: make([]bool, n),
		paths:     make([][]int, n),
		parts:     make([]pathPart, n),
	}
}

// add records field i, which binds the member name.
func (of *objectFields) add(i int, name string, t reflect.Type, omitEmpty bool, path []int, goName string) {
	of.index[name] = i
	of.plans[i] = planFor(t)
	of.omitEmpty[i] = omitEmpty
	of.paths[i] = path
	of.parts[i] = pathPart{field: goName, name: name}
}

// sentFields records which fields an object sent: in a word for the first 64,
// allocating only for a member of a wider struct.
type sentFields struct {
	bits uint64
	more []bool
}

func (s *sentFields) add(i int) {
	if i < 64 {
		s.bits |= 1 << i
		return
	}
	for len(s.more) <= i-64 {
		s.more = append(s.more, false)
	}
	s.more[i-64] = true
}

func (s *sentFields) has(i int) bool {
	if i < 64 {
		return s.bits&(1<<i) != 0
	}
	return i-64 < len(s.more) && s.more[i-64]
}

// decodeFields decodes the members of an object, its opening brace read, into
// the fields of v that of describes, through its closing brace. It returns
// which fields were sent and, once any has failed, each field's failures,
// both indexed as of is. With unknown given, it appends the name of each
// distinct member no field binds, up to maxFailures of them.
//
// A member sent twice binds its last occurrence, unless an earlier one
// failed: as in encoding/json, that failure stands. Past the failure budget,
// after a failed occurrence of the member, or for a null, which sets nothing,
// a value is only checked for being well-formed.
func decodeFields(dec *jsonDecoder, of *objectFields, v reflect.Value, unknown *[]string) (sent sentFields, failures []BindErrors, err error) {
	for dec.PeekKind() == '"' {
		// The name is looked up from its raw bytes, since a map lookup keyed
		// by string(b) does not allocate, where making it a string first
		// would, for every member. The bytes are voided by the next call on
		// the decoder, so an unknown name is copied before moving on.
		raw, err := dec.ReadValue()
		if err != nil {
			return sent, nil, err
		}
		name, err := memberName(raw)
		if err != nil {
			return sent, nil, err
		}
		i, ok := of.index[string(name)]
		if !ok {
			// Only as many as are reported are kept, so checking for a
			// repeat is a scan of at most maxFailures names.
			if unknown != nil && len(*unknown) < maxFailures && !slices.Contains(*unknown, string(name)) {
				*unknown = append(*unknown, string(name))
			}
			if err := dec.SkipValue(); err != nil {
				return sent, nil, err
			}
			continue
		}

		sent.add(i)
		if dec.exhausted() || failures != nil && len(failures[i]) > 0 || dec.PeekKind() == 'n' {
			if err := dec.SkipValue(); err != nil {
				return sent, nil, err
			}
			continue
		}
		// An omitempty field is reached only once its value is known not to
		// be empty, so an empty one allocates no embedded pointer on the way.
		if of.omitEmpty[i] {
			err = decodeInto(dec, of.plans[i], dec.PeekKind(), true, func() reflect.Value { return fieldAt(v, of.paths[i]) })
		} else {
			err = decodeWithPlan(dec, fieldAt(v, of.paths[i]), of.plans[i])
		}
		if err != nil {
			if failures == nil {
				failures = make([]BindErrors, len(of.plans))
			}
			if err := collectFailure(&failures[i], err, of.parts[i]); err != nil {
				return sent, nil, err
			}
		}
	}
	_, err = dec.ReadToken() // the closing brace
	return sent, failures, err
}

// nestedStruct is what decoding a nested struct from a JSON object needs,
// resolved once per type: its fields, and which of them are required.
type nestedStruct struct {
	objectFields
	required []bool
}

var nestedStructCache sync.Map // reflect.Type -> *nestedStruct

func nestedStructFor(typ reflect.Type) *nestedStruct {
	if cached, ok := nestedStructCache.Load(typ); ok {
		return cached.(*nestedStruct)
	}
	fields := nestedFieldsFor(typ)
	ns := &nestedStruct{objectFields: newObjectFields(len(fields)), required: make([]bool, len(fields))}
	for i, tf := range fields {
		ns.add(i, tf.name, tf.field.Type, omitsEmpty(tf.field.Type, tf.opts), tf.index, tf.goName)
		ns.required[i] = hasOption(tf.opts, optRequired)
	}
	nestedStructCache.Store(typ, ns)
	return ns
}

// decodeFast sets a predeclared field straight from a token of the matching
// kind, reporting whether it did. With omitEmpty an empty value sets nothing,
// and target is called only for a value that will be set.
func decodeFast(dec *jsonDecoder, fast fastKind, kind jsontext.Kind, omitEmpty bool, target func() reflect.Value) (bool, error) {
	switch fast {
	case fastString:
		if kind == '"' {
			tok, err := dec.ReadToken()
			if err != nil {
				return true, err
			}
			if text := tok.String(); !omitEmpty || text != "" {
				target().SetString(text)
			}
			return true, nil
		}

	case fastBool:
		if kind == 't' || kind == 'f' {
			tok, err := dec.ReadToken()
			if err != nil {
				return true, err
			}
			if value := tok.Bool(); !omitEmpty || value {
				target().SetBool(value)
			}
			return true, nil
		}

	case fastInt, fastUint, fastFloat:
		if kind == '0' {
			return true, decodeNumber(dec, fast, omitEmpty, target)
		}
	}
	return false, nil
}

// decodeNumber writes a numeric token into a predeclared numeric field. A
// literal an exact parse rejects, such as 1e3 for an integer, is handed to
// the general conversion, which accepts it as earlier releases did.
func decodeNumber(dec *jsonDecoder, fast fastKind, omitEmpty bool, target func() reflect.Value) error {
	raw, err := dec.ReadValue()
	if err != nil {
		return err
	}
	switch fast {
	case fastInt:
		number, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return setFieldFromNumber(dec, raw, omitEmpty, target)
		}
		if omitEmpty && number == 0 {
			return nil
		}
		return dec.fail(setIntChecked(target(), number))

	case fastUint:
		number, err := strconv.ParseUint(string(raw), 10, 64)
		if err != nil {
			return setFieldFromNumber(dec, raw, omitEmpty, target)
		}
		if omitEmpty && number == 0 {
			return nil
		}
		return dec.fail(setUintChecked(target(), number))

	default: // fastFloat
		number, err := strconv.ParseFloat(string(raw), 64)
		if err != nil {
			return setFieldFromNumber(dec, raw, omitEmpty, target)
		}
		if omitEmpty && number == 0 {
			return nil
		}
		return dec.fail(setFloatChecked(target(), number))
	}
}

// setFieldFromNumber converts a number whose literal an exact parse rejected,
// such as 1e3 into an integer field.
func setFieldFromNumber(dec *jsonDecoder, raw jsontext.Value, omitEmpty bool, target func() reflect.Value) error {
	number := json.Number(raw.String())
	if omitEmpty && isEmptyValue(number) {
		return nil
	}
	return dec.fail(setField(target(), number))
}

// conversionFailure marks a failure to convert a decoded value as concerning
// one field, so that it is reported as a BindError naming it rather than as a
// malformed body. A syntax error from the decoder is a different thing and
// stays as it is.
type conversionFailure struct{ err error }

func (c conversionFailure) Error() string { return c.err.Error() }
