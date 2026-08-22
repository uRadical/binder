package binder

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func numReq(t *testing.T, bodyJSON string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/u", strings.NewReader(bodyJSON))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// Integers beyond 2^53 must survive binding. Routed through float64 they do
// not: 9007199254740993 comes back as 9007199254740992.
func TestLargeInt64KeepsPrecision(t *testing.T) {
	tests := []struct {
		json string
		want int64
	}{
		{"9007199254740993", 9007199254740993},  // 2^53 + 1
		{"9223372036854775807", math.MaxInt64},  // largest int64
		{"-9223372036854775808", math.MinInt64}, // smallest int64
		{"1234567890123456789", 1234567890123456789},
	}

	for _, tt := range tests {
		var got struct {
			ID int64 `body:"id"`
		}
		if err := Bind(numReq(t, `{"id":`+tt.json+`}`), &got); err != nil {
			t.Fatalf("binding %s: %v", tt.json, err)
		}
		if got.ID != tt.want {
			t.Errorf("id %s bound as %d, want %d", tt.json, got.ID, tt.want)
		}
	}
}

func TestLargeUint64KeepsPrecision(t *testing.T) {
	var got struct {
		ID uint64 `body:"id"`
	}
	// Above MaxInt64, so this cannot round-trip through int64 either.
	if err := Bind(numReq(t, `{"id":18446744073709551615}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.ID != math.MaxUint64 {
		t.Errorf("ID = %d, want %d", got.ID, uint64(math.MaxUint64))
	}
}

// Numbers written in exponent or decimal form were accepted through float64
// before and must still be.
func TestNumbersInFloatFormStillBindToInt(t *testing.T) {
	tests := []struct {
		json string
		want int
	}{
		{"1e5", 100000},
		{"1.0", 1},
		{"2.9", 2},
		{"-1e3", -1000},
	}

	for _, tt := range tests {
		var got struct {
			N int `body:"n"`
		}
		if err := Bind(numReq(t, `{"n":`+tt.json+`}`), &got); err != nil {
			t.Fatalf("binding %s: %v", tt.json, err)
		}
		if got.N != tt.want {
			t.Errorf("n %s bound as %d, want %d", tt.json, got.N, tt.want)
		}
	}
}

func TestFloatFieldsUnaffected(t *testing.T) {
	var got struct {
		Score float64 `body:"score"`
		Ratio float32 `body:"ratio"`
	}
	if err := Bind(numReq(t, `{"score":98.6,"ratio":0.5}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Score != 98.6 {
		t.Errorf("Score = %v, want 98.6", got.Score)
	}
	if got.Ratio != 0.5 {
		t.Errorf("Ratio = %v, want 0.5", got.Ratio)
	}
}

func TestNumberToBool(t *testing.T) {
	for _, tt := range []struct {
		json string
		want bool
	}{{"1", true}, {"0", false}, {"2.5", true}} {
		var got struct {
			Flag bool `body:"flag"`
		}
		if err := Bind(numReq(t, `{"flag":`+tt.json+`}`), &got); err != nil {
			t.Fatalf("binding %s: %v", tt.json, err)
		}
		if got.Flag != tt.want {
			t.Errorf("flag %s bound as %v, want %v", tt.json, got.Flag, tt.want)
		}
	}
}

// A large number bound into a string field keeps its digits rather than
// arriving in scientific notation via float64.
func TestNumberToStringKeepsDigits(t *testing.T) {
	var got struct {
		ID string `body:"id"`
	}
	if err := Bind(numReq(t, `{"id":123456789012345678}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.ID != "123456789012345678" {
		t.Errorf("ID = %q, want %q", got.ID, "123456789012345678")
	}
}

// omitempty judges a number by its value, not by the length of its text.
func TestOmitEmptyTreatsZeroNumberAsEmpty(t *testing.T) {
	var got struct {
		N int `body:"n,omitempty"`
	}
	got.N = 42

	if err := Bind(numReq(t, `{"n":0}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.N != 42 {
		t.Errorf("N = %d, want 42 - a zero number is empty for omitempty", got.N)
	}
}

func TestOmitEmptyBindsNonZeroNumber(t *testing.T) {
	var got struct {
		N int `body:"n,omitempty"`
	}
	got.N = 42

	if err := Bind(numReq(t, `{"n":7}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.N != 7 {
		t.Errorf("N = %d, want 7", got.N)
	}
}

// Precision must survive nested structs and slices too.
func TestLargeIntInNestedStructAndSlice(t *testing.T) {
	type inner struct {
		ID int64 `body:"id"`
	}
	var got struct {
		Nested inner   `body:"nested"`
		IDs    []int64 `body:"ids"`
	}

	body := `{"nested":{"id":9007199254740993},"ids":[9007199254740993,9007199254740995]}`
	if err := Bind(numReq(t, body), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Nested.ID != 9007199254740993 {
		t.Errorf("nested ID = %d, want 9007199254740993", got.Nested.ID)
	}
	if len(got.IDs) != 2 || got.IDs[0] != 9007199254740993 || got.IDs[1] != 9007199254740995 {
		t.Errorf("IDs = %v, want [9007199254740993 9007199254740995]", got.IDs)
	}
}

// Form values are strings and are unaffected by the change.
func TestFormNumbersUnaffected(t *testing.T) {
	var got struct {
		ID int64 `body:"id"`
	}
	r := httptest.NewRequest("POST", "/u", strings.NewReader("id=9007199254740993"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.ID != 9007199254740993 {
		t.Errorf("ID = %d, want 9007199254740993", got.ID)
	}
}
