// Copyright 2025 The binder Authors.

//go:build goexperiment.jsonv2

package binder

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
)

// decodeJSONObject reads a JSON object into the representation binding works
// with. It walks tokens rather than unmarshalling into a map, which lets it
// skip the value of any top-level member no field binds, and keeps numbers as
// their literal text so that precision beyond 2^53 survives.
//
// A nil wanted set decodes every member, which is what the unknown-field check
// needs in order to see the members nothing binds.
func decodeJSONObject(data []byte, wanted map[string]struct{}) (map[string]interface{}, error) {
	// Duplicate names are accepted, and the last wins, as encoding/json does.
	dec := jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowDuplicateNames(true))

	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	switch tok.Kind() {
	case 'n':
		// A literal null body carries no members, and is not an error.
		return make(map[string]interface{}), nil
	case '{':
	default:
		return nil, fmt.Errorf("cannot unmarshal %s into map[string]interface {}", jsonKindName(tok.Kind()))
	}

	out := make(map[string]interface{})
	for dec.PeekKind() == '"' {
		nameTok, err := dec.ReadToken()
		if err != nil {
			return nil, err
		}
		// A Token is voided by the next call on the decoder, so the name has
		// to be taken before the value is read.
		name := nameTok.String()

		if wanted != nil {
			if _, ok := wanted[name]; !ok {
				if err := dec.SkipValue(); err != nil {
					return nil, err
				}
				continue
			}
		}

		value, err := decodeJSONValue(dec)
		if err != nil {
			return nil, err
		}
		out[name] = value
	}

	if _, err := dec.ReadToken(); err != nil { // the closing brace
		return nil, err
	}
	return out, nil
}

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
