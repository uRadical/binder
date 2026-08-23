// Copyright 2025 The binder Authors.

//go:build !goexperiment.jsonv2

package binder

import (
	"bytes"
	"encoding/json"
)

// decodeJSONObject reads a JSON object into the representation binding works
// with.
//
// This is the fallback for a toolchain built with GOEXPERIMENT=nojsonv2, where
// encoding/json/jsontext is unavailable. It produces the same values as the
// token-walking implementation, but decodes every member rather than skipping
// those no field binds, so wanted is ignored.
func decodeJSONObject(data []byte, wanted map[string]struct{}) (map[string]interface{}, error) {
	_ = wanted // no selective decoding without jsontext

	var out map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(data))
	// Decode numbers as their literal text. Routing them through float64
	// silently loses precision beyond 2^53, so an identifier such as
	// 9007199254740993 would bind as 9007199254740992.
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if out == nil {
		// A literal null body carries no members, and is not an error.
		out = make(map[string]interface{})
	}
	return out, nil
}
