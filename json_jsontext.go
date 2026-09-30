// Copyright 2025 The binder Authors.

package binder

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
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
	dec *jsontext.Decoder
}

var decoderPool = sync.Pool{
	New: func() any {
		p := new(pooledDecoder)
		p.dec = jsontext.NewDecoder(&p.buf, decodeOptions)
		return p
	},
}

// getDecoder returns a decoder reading data. Release it with putDecoder once
// nothing read from it is still in use.
func getDecoder(data []byte) *pooledDecoder {
	p := decoderPool.Get().(*pooledDecoder)
	p.buf = *bytes.NewBuffer(data)
	p.dec.Reset(&p.buf, decodeOptions)
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
func decodeJSONValue(dec *jsontext.Decoder) (any, error) {
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

// bindJSONBody fills a struct's body-sourced fields straight from the request
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
func jsonBodyInto(data []byte, info *typeInfo, val reflect.Value, wanted map[string]struct{}, wantUnknown bool, bound []bool, errs *fieldErrs) (bodyData map[string]any, unknown []string, err error) {
	_ = wanted // the walk consults info.bodyFields directly
	pooled := getDecoder(data)
	defer putDecoder(pooled)
	dec := pooled.dec

	tok, err := dec.ReadToken()
	if err != nil {
		return nil, nil, err
	}
	switch tok.Kind() {
	case 'n':
		// A null body carries no members.
		return nil, nil, endOfBody(dec)
	case '{':
	default:
		return nil, nil, fmt.Errorf("cannot unmarshal %s into map[string]interface {}", jsonKindName(tok.Kind()))
	}

	for dec.PeekKind() == '"' {
		// The name is looked up from its raw bytes, since a map lookup keyed
		// by string(b) does not allocate, where making it a string first
		// would, for every member. The bytes are voided by the next call on
		// the decoder, so an unknown name is copied before moving on.
		raw, err := dec.ReadValue()
		if err != nil {
			return nil, nil, err
		}
		name, err := memberName(raw)
		if err != nil {
			return nil, nil, err
		}

		index, isBound := info.bodyFields[string(name)]
		if !isBound {
			if wantUnknown && !slices.Contains(unknown, string(name)) {
				unknown = append(unknown, string(name))
			}
			if err := dec.SkipValue(); err != nil {
				return nil, nil, err
			}
			continue
		}

		// A conversion failure has already consumed the member's value, so
		// the walk can carry on to the next one. The field counts as bound
		// either way: it was present, so required must not report it again.
		fi := info.fields[index]
		if err := decodeJSONInto(dec, fieldByIndex(val, fi.Index), fi); err != nil {
			var failed conversionFailure
			if !errors.As(err, &failed) {
				return nil, nil, err
			}
			errs.set(info, index, fieldFailures(fi, failed.err))
		}
		bound[index] = true
	}

	if _, err := dec.ReadToken(); err != nil { // the closing brace
		return nil, nil, err
	}
	return nil, unknown, endOfBody(dec)
}

// endOfBody reports anything after the body's one JSON value, such as a
// second object, which would otherwise be ignored. Trailing whitespace is
// allowed.
func endOfBody(dec *jsontext.Decoder) error {
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
func decodeJSONInto(dec *jsontext.Decoder, field reflect.Value, fi fieldInfo) error {
	kind := dec.PeekKind()

	switch fi.Fast {
	case fastString:
		if kind == '"' {
			tok, err := dec.ReadToken()
			if err != nil {
				return err
			}
			text := tok.String()
			if fi.OmitEmpty && text == "" {
				return nil
			}
			field.SetString(text)
			return nil
		}

	case fastBool:
		if kind == 't' || kind == 'f' {
			tok, err := dec.ReadToken()
			if err != nil {
				return err
			}
			value := tok.Bool()
			if fi.OmitEmpty && !value {
				return nil
			}
			field.SetBool(value)
			return nil
		}

	case fastInt, fastUint, fastFloat:
		if kind == '0' {
			return decodeNumberInto(dec, field, fi)
		}
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
	return conversionError(setField(field, value))
}

// decodeNumberInto writes a numeric token into a predeclared numeric field.
// A literal an exact parse rejects, such as 1e3 for an integer, is handed to
// the general conversion, which accepts it as earlier releases did.
func decodeNumberInto(dec *jsontext.Decoder, field reflect.Value, fi fieldInfo) error {
	raw, err := dec.ReadValue()
	if err != nil {
		return err
	}

	switch fi.Fast {
	case fastInt:
		number, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return setFieldFromNumber(field, raw, fi)
		}
		if fi.OmitEmpty && number == 0 {
			return nil
		}
		return conversionError(setIntChecked(field, number))

	case fastUint:
		number, err := strconv.ParseUint(string(raw), 10, 64)
		if err != nil {
			return setFieldFromNumber(field, raw, fi)
		}
		if fi.OmitEmpty && number == 0 {
			return nil
		}
		return conversionError(setUintChecked(field, number))

	default: // fastFloat
		number, err := strconv.ParseFloat(string(raw), 64)
		if err != nil {
			return setFieldFromNumber(field, raw, fi)
		}
		if fi.OmitEmpty && number == 0 {
			return nil
		}
		return conversionError(setFloatChecked(field, number))
	}
}

// setFieldFromNumber converts a number whose literal an exact parse rejected,
// such as 1e3 into an integer field.
func setFieldFromNumber(field reflect.Value, raw jsontext.Value, fi fieldInfo) error {
	number := json.Number(raw.String())
	if fi.OmitEmpty && isEmptyValue(number) {
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
