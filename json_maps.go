// Copyright 2025 The binder Authors.

//go:build !goexperiment.jsonv2

package binder

import (
	"bytes"
	"encoding/json"
	"reflect"
)

// jsonBodyInto reads a JSON body into the representation binding works with.
//
// This is the fallback for a toolchain built with GOEXPERIMENT=nojsonv2, where
// encoding/json/jsontext is unavailable. Without a token decoder it cannot
// write into fields directly or skip the members nothing binds, so it returns
// the map for the caller to bind from, as earlier releases did. The values it
// produces are the same either way.
func jsonBodyInto(data []byte, info *typeInfo, val reflect.Value, wanted map[string]struct{}, wantUnknown bool) (map[string]interface{}, []bool, []string, error) {
	_ = info
	_ = val
	_ = wanted      // no selective decoding without jsontext
	_ = wantUnknown // the caller checks the returned map instead

	var out map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(data))
	// Decode numbers as their literal text. Routing them through float64
	// silently loses precision beyond 2^53, so an identifier such as
	// 9007199254740993 would bind as 9007199254740992.
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, nil, nil, err
	}
	if out == nil {
		// A literal null body carries no members, and is not an error.
		out = make(map[string]interface{})
	}
	return out, nil, nil, nil
}
