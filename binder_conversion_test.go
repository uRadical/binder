package binder

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

// settable returns an addressable zero Value of the given kind's type, so the
// set* helpers can be exercised directly.
func settable(v interface{}) reflect.Value {
	return reflect.New(reflect.TypeOf(v)).Elem()
}

func TestSetIntFromEveryKind(t *testing.T) {
	tests := []struct {
		name    string
		value   interface{}
		want    int64
		wantErr bool
	}{
		{"int", int(7), 7, false},
		{"int8", int8(7), 7, false},
		{"int16", int16(7), 7, false},
		{"int32", int32(7), 7, false},
		{"int64", int64(7), 7, false},
		{"float32", float32(7.9), 7, false},
		{"float64", float64(7.9), 7, false},
		{"json.Number integer", json.Number("7"), 7, false},
		{"json.Number beyond float64", json.Number("9007199254740993"), 9007199254740993, false},
		{"json.Number exponent", json.Number("1e3"), 1000, false},
		{"json.Number decimal", json.Number("2.9"), 2, false},
		{"json.Number invalid", json.Number("nope"), 0, true},
		{"string", "7", 7, false},
		{"string invalid", "seven", 0, true},
		{"unsupported", []string{"7"}, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := settable(int64(0))
			err := setInt(field, tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("setInt(%v) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if err == nil && field.Int() != tt.want {
				t.Errorf("setInt(%v) = %d, want %d", tt.value, field.Int(), tt.want)
			}
		})
	}
}

func TestSetUintFromEveryKind(t *testing.T) {
	tests := []struct {
		name    string
		value   interface{}
		want    uint64
		wantErr bool
	}{
		{"uint", uint(7), 7, false},
		{"uint8", uint8(7), 7, false},
		{"uint16", uint16(7), 7, false},
		{"uint32", uint32(7), 7, false},
		{"uint64", uint64(7), 7, false},
		{"int", int(7), 7, false},
		{"int negative", int(-1), 0, true},
		{"float64", float64(7.9), 7, false},
		{"float64 negative", float64(-1), 0, true},
		{"json.Number", json.Number("7"), 7, false},
		{"json.Number above MaxInt64", json.Number("18446744073709551615"), math.MaxUint64, false},
		{"json.Number exponent", json.Number("1e3"), 1000, false},
		{"json.Number negative", json.Number("-1"), 0, true},
		{"json.Number invalid", json.Number("nope"), 0, true},
		{"string", "7", 7, false},
		{"string invalid", "seven", 0, true},
		{"unsupported", []string{"7"}, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := settable(uint64(0))
			err := setUint(field, tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("setUint(%v) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if err == nil && field.Uint() != tt.want {
				t.Errorf("setUint(%v) = %d, want %d", tt.value, field.Uint(), tt.want)
			}
		})
	}
}

func TestSetFloatFromEveryKind(t *testing.T) {
	tests := []struct {
		name    string
		value   interface{}
		want    float64
		wantErr bool
	}{
		{"float32", float32(0.5), 0.5, false},
		{"float64", float64(0.5), 0.5, false},
		{"int", int(7), 7, false},
		{"int8", int8(7), 7, false},
		{"int16", int16(7), 7, false},
		{"int32", int32(7), 7, false},
		{"int64", int64(7), 7, false},
		{"json.Number", json.Number("0.5"), 0.5, false},
		{"json.Number invalid", json.Number("nope"), 0, true},
		{"string", "0.5", 0.5, false},
		{"string invalid", "half", 0, true},
		{"unsupported", []string{"1"}, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := settable(float64(0))
			err := setFloat(field, tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("setFloat(%v) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if err == nil && field.Float() != tt.want {
				t.Errorf("setFloat(%v) = %v, want %v", tt.value, field.Float(), tt.want)
			}
		})
	}
}

func TestSetBoolFromEveryKind(t *testing.T) {
	tests := []struct {
		name    string
		value   interface{}
		want    bool
		wantErr bool
	}{
		{"bool true", true, true, false},
		{"bool false", false, false, false},
		{"string true", "true", true, false},
		{"string invalid", "yes please", false, true},
		{"int nonzero", int(1), true, false},
		{"int zero", int(0), false, false},
		{"float64 nonzero", float64(2.5), true, false},
		{"float64 zero", float64(0), false, false},
		{"json.Number nonzero", json.Number("1"), true, false},
		{"json.Number zero", json.Number("0"), false, false},
		{"json.Number invalid", json.Number("nope"), false, true},
		{"unsupported", []string{"true"}, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := settable(false)
			err := setBool(field, tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("setBool(%v) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if err == nil && field.Bool() != tt.want {
				t.Errorf("setBool(%v) = %v, want %v", tt.value, field.Bool(), tt.want)
			}
		})
	}
}

func TestIsEmptyValueForEveryKind(t *testing.T) {
	var nilPtr *int
	var nilIface interface{}
	nonNil := 1

	tests := []struct {
		name  string
		value interface{}
		want  bool
	}{
		{"nil", nil, true},
		{"json.Number zero", json.Number("0"), true},
		{"json.Number zero float", json.Number("0.0"), true},
		{"json.Number nonzero", json.Number("1"), false},
		{"json.Number invalid", json.Number("nope"), false},
		{"empty string", "", true},
		{"string", "a", false},
		{"empty array", [0]int{}, true},
		{"array", [1]int{1}, false},
		{"nil map", map[string]int(nil), true},
		{"empty map", map[string]int{}, true},
		{"map", map[string]int{"a": 1}, false},
		{"nil slice", []int(nil), true},
		{"empty slice", []int{}, true},
		{"slice", []int{1}, false},
		{"false", false, true},
		{"true", true, false},
		{"zero int", int(0), true},
		{"int", int(1), false},
		{"zero int8", int8(0), true},
		{"zero uint", uint(0), true},
		{"uint", uint(1), false},
		{"zero uintptr", uintptr(0), true},
		{"zero float", float64(0), true},
		{"float", float64(1), false},
		{"nil pointer", nilPtr, true},
		{"pointer", &nonNil, false},
		{"nil interface", nilIface, true},
		{"struct", struct{ A int }{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isEmptyValue(tt.value); got != tt.want {
				t.Errorf("isEmptyValue(%#v) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

type stringerValue struct{}

func (stringerValue) String() string { return "stringer" }

func TestToStringFromEveryKind(t *testing.T) {
	tests := []struct {
		name  string
		value interface{}
		want  string
	}{
		{"string", "a", "a"},
		{"bytes", []byte("a"), "a"},
		{"stringer", stringerValue{}, "stringer"},
		{"json.Number", json.Number("123456789012345678"), "123456789012345678"},
		{"int", 7, "7"},
		{"bool", true, "true"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := toString(tt.value)
			if err != nil {
				t.Fatalf("toString(%v) error = %v", tt.value, err)
			}
			if got != tt.want {
				t.Errorf("toString(%v) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

// Arrays are refused with an explanation rather than silently ignored.
func TestSetFieldByKindUnsupported(t *testing.T) {
	field := settable([2]int{})
	if err := setFieldByKind(field, []interface{}{1, 2}); err == nil {
		t.Error("array field: got nil error, want a refusal")
	}

	ch := settable(make(chan int))
	if err := setFieldByKind(ch, "x"); err == nil {
		t.Error("channel field: got nil error, want a refusal")
	}
}
