// Copyright 2025 The binder Authors.

//go:build goexperiment.jsonv2

package binder

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"reflect"
	"strconv"
)

// decodeJSONValue reads one value, producing the types the set* helpers
// expect: string, bool, json.Number, []interface{}, map[string]interface{}
// and nil.
func decodeJSONValue(dec *jsontext.Decoder) (interface{}, error) {
	switch dec.PeekKind() {
	case '{':
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		object := make(map[string]interface{})
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
		var array []interface{}
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
// Going through map[string]interface{} costs an allocation per member for the
// interface value, plus the map itself, before any conversion happens. Walking
// the tokens once and writing into the destination as each member is reached
// avoids both, and lets a member no field binds be skipped without decoding it
// at all.
//
// It reports which fields were filled, so the caller can apply required to the
// ones that were not, and the names of members nothing binds.
func jsonBodyInto(data []byte, info *typeInfo, val reflect.Value, wanted map[string]struct{}, wantUnknown bool) (bodyData map[string]interface{}, bound []bool, unknown []string, err error) {
	_ = wanted // the walk consults info.bodyFields directly
	dec := jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowDuplicateNames(true))

	tok, err := dec.ReadToken()
	if err != nil {
		return nil, nil, nil, err
	}
	switch tok.Kind() {
	case 'n':
		return nil, nil, nil, nil // a null body carries no members
	case '{':
	default:
		return nil, nil, nil, fmt.Errorf("cannot unmarshal %s into map[string]interface {}", jsonKindName(tok.Kind()))
	}

	bound = make([]bool, len(info.fields))
	for dec.PeekKind() == '"' {
		nameTok, err := dec.ReadToken()
		if err != nil {
			return nil, nil, nil, err
		}
		// A Token is voided by the next call on the decoder.
		name := nameTok.String()

		index, isBound := info.bodyFields[name]
		if !isBound {
			if wantUnknown {
				unknown = append(unknown, name)
			}
			if err := dec.SkipValue(); err != nil {
				return nil, nil, nil, err
			}
			continue
		}

		fi := info.fields[index]
		if err := decodeJSONInto(dec, val.Field(fi.Index), fi); err != nil {
			return nil, nil, nil, err
		}
		bound[index] = true
	}

	if _, err := dec.ReadToken(); err != nil { // the closing brace
		return nil, nil, nil, err
	}
	return nil, bound, unknown, nil
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
	return conversionError(fi, setField(field, value))
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
		return conversionError(fi, setIntChecked(field, number))

	case fastUint:
		number, err := strconv.ParseUint(string(raw), 10, 64)
		if err != nil {
			return setFieldFromNumber(field, raw, fi)
		}
		if fi.OmitEmpty && number == 0 {
			return nil
		}
		return conversionError(fi, setUintChecked(field, number))

	default: // fastFloat
		number, err := strconv.ParseFloat(string(raw), 64)
		if err != nil {
			return setFieldFromNumber(field, raw, fi)
		}
		if fi.OmitEmpty && number == 0 {
			return nil
		}
		return conversionError(fi, setFloatChecked(field, number))
	}
}

// setFieldFromNumber converts a number whose literal an exact parse rejected,
// such as 1e3 into an integer field.
func setFieldFromNumber(field reflect.Value, raw jsontext.Value, fi fieldInfo) error {
	number := json.Number(raw.String())
	if fi.OmitEmpty && isEmptyValue(number) {
		return nil
	}
	return conversionError(fi, setField(field, number))
}

// conversionError marks a failure to convert a decoded value as concerning one
// field, so that it is reported as a BindError naming it rather than as a
// malformed body. A syntax error from the decoder is a different thing and
// stays as it is.
func conversionError(fi fieldInfo, err error) error {
	if err == nil {
		return nil
	}
	return newBindError(fi, fmt.Sprintf("error setting field %s: %v", fi.FieldType.Name, err), err)
}
