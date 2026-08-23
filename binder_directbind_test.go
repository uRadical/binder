package binder

import (
	"errors"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A JSON body is bound straight into fields where the build allows it, and
// through a map otherwise. Both must agree, so every case here runs under
// GOEXPERIMENT=nojsonv2 as well; the suite passing both ways is the check.

func bindBody(t *testing.T, body string, target interface{}) error {
	t.Helper()
	r := httptest.NewRequest("POST", "/u", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return Bind(r, target)
}

// Each predeclared kind takes a direct path when the token matches it.
func TestDirectBindMatchingKinds(t *testing.T) {
	var got struct {
		S   string  `body:"s"`
		I   int     `body:"i"`
		I8  int8    `body:"i8"`
		U   uint    `body:"u"`
		U8  uint8   `body:"u8"`
		F32 float32 `body:"f32"`
		F64 float64 `body:"f64"`
		B   bool    `body:"b"`
	}

	body := `{"s":"text","i":-42,"i8":-8,"u":42,"u8":8,"f32":1.5,"f64":2.5,"b":true}`
	if err := bindBody(t, body, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.S != "text" || got.I != -42 || got.I8 != -8 || got.U != 42 || got.U8 != 8 {
		t.Errorf("bound %+v", got)
	}
	if got.F32 != 1.5 || got.F64 != 2.5 || !got.B {
		t.Errorf("bound %+v", got)
	}
}

// Where the token does not match the destination, the general conversion runs,
// so binder's coercions keep working.
func TestDirectBindCoercions(t *testing.T) {
	var got struct {
		I int     `body:"i"`
		U uint    `body:"u"`
		F float64 `body:"f"`
		B bool    `body:"b"`
		S string  `body:"s"`
	}

	// Numbers as strings, and a number into a string field.
	body := `{"i":"42","u":"42","f":"1.5","b":"true","s":99}`
	if err := bindBody(t, body, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.I != 42 || got.U != 42 || got.F != 1.5 || !got.B {
		t.Errorf("bound %+v, want the string forms converted", got)
	}
	if got.S != "99" {
		t.Errorf("S = %q, want %q", got.S, "99")
	}
}

// Numbers written in a form an exact parse rejects fall back to the float
// conversion, as they did before.
func TestDirectBindExponentAndDecimalForms(t *testing.T) {
	var got struct {
		A int  `body:"a"`
		B int  `body:"b"`
		C uint `body:"c"`
	}
	if err := bindBody(t, `{"a":1e3,"b":2.0,"c":1e2}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.A != 1000 || got.B != 2 || got.C != 100 {
		t.Errorf("bound %+v, want a=1000 b=2 c=100", got)
	}
}

// Precision beyond float64 survives the direct path.
func TestDirectBindLargeIntegers(t *testing.T) {
	var got struct {
		I int64  `body:"i"`
		U uint64 `body:"u"`
	}
	body := `{"i":9007199254740993,"u":18446744073709551615}`
	if err := bindBody(t, body, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.I != 9007199254740993 {
		t.Errorf("I = %d, want 9007199254740993", got.I)
	}
	if got.U != math.MaxUint64 {
		t.Errorf("U = %d, want %d", got.U, uint64(math.MaxUint64))
	}
}

// Out-of-range values are refused rather than truncated.
func TestDirectBindOutOfRange(t *testing.T) {
	var got struct {
		I8 int8 `body:"i8"`
	}
	if err := bindBody(t, `{"i8":9999}`, &got); err == nil {
		t.Error("9999 into int8: got nil error, want a refusal")
	}

	var negative struct {
		U uint `body:"u"`
	}
	if err := bindBody(t, `{"u":-1}`, &negative); err == nil {
		t.Error("-1 into uint: got nil error, want a refusal")
	}
}

// omitempty must behave the same on the direct path as through a map.
func TestDirectBindOmitEmpty(t *testing.T) {
	var got struct {
		S string  `body:"s,omitempty"`
		I int     `body:"i,omitempty"`
		U uint    `body:"u,omitempty"`
		F float64 `body:"f,omitempty"`
		B bool    `body:"b,omitempty"`
	}
	got.S, got.I, got.U, got.F, got.B = "keep", 7, 7, 7, true

	if err := bindBody(t, `{"s":"","i":0,"u":0,"f":0,"b":false}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.S != "keep" || got.I != 7 || got.U != 7 || got.F != 7 || !got.B {
		t.Errorf("bound %+v, want every field left untouched", got)
	}
}

func TestDirectBindOmitEmptyPassesNonZero(t *testing.T) {
	var got struct {
		S string `body:"s,omitempty"`
		I int    `body:"i,omitempty"`
	}
	got.S, got.I = "old", 1

	if err := bindBody(t, `{"s":"new","i":2}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.S != "new" || got.I != 2 {
		t.Errorf("bound %+v, want the new values", got)
	}
}

// A named type may define UnmarshalText, so it must not take a fast path.
type upperString string

func (u *upperString) UnmarshalText(b []byte) error {
	*u = upperString(strings.ToUpper(string(b)))
	return nil
}

func TestDirectBindNamedTypeUsesUnmarshaler(t *testing.T) {
	var got struct {
		S upperString `body:"s"`
	}
	if err := bindBody(t, `{"s":"shout"}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.S != "SHOUT" {
		t.Errorf("S = %q, want %q - the type's own unmarshaler must run", got.S, "SHOUT")
	}
}

// A named type without an unmarshaler still binds, through the general path.
type plainString string

func TestDirectBindPlainNamedType(t *testing.T) {
	var got struct {
		S plainString `body:"s"`
	}
	if err := bindBody(t, `{"s":"value"}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.S != "value" {
		t.Errorf("S = %q, want %q", got.S, "value")
	}
}

func TestDirectBindTextUnmarshalerStruct(t *testing.T) {
	var got struct {
		When time.Time `body:"when"`
	}
	if err := bindBody(t, `{"when":"2026-01-02T03:04:05Z"}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if !got.When.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("When = %v", got.When)
	}
}

// A conversion failure names the field rather than blaming the body.
func TestDirectBindConversionErrorNamesField(t *testing.T) {
	var got struct {
		Count int `body:"count"`
	}
	err := bindBody(t, `{"count":"not a number"}`, &got)
	if err == nil {
		t.Fatal("got nil error")
	}

	var bindErr *BindError
	if !errors.As(err, &bindErr) {
		t.Fatalf("errors.As(*BindError) = false for %v", err)
	}
	if bindErr.Field != "Count" {
		t.Errorf("Field = %q, want %q", bindErr.Field, "Count")
	}
	if errors.Is(err, ErrMalformedBody) {
		t.Error("a conversion failure was reported as a malformed body")
	}
}

// A null member leaves its field alone.
func TestDirectBindNullMember(t *testing.T) {
	var got struct {
		S string `body:"s"`
		I int    `body:"i"`
	}
	got.S, got.I = "keep", 7

	if err := bindBody(t, `{"s":null,"i":null}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.S != "keep" || got.I != 7 {
		t.Errorf("bound %+v, want both left untouched", got)
	}
}

// required still fires for a member the body omits, and is satisfied by one
// it carries.
func TestDirectBindRequired(t *testing.T) {
	var missing struct {
		S string `body:"s,required"`
	}
	if err := bindBody(t, `{"other":1}`, &missing); !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("got %v, want ErrMissingRequired", err)
	}

	var present struct {
		S string `body:"s,required"`
	}
	if err := bindBody(t, `{"s":""}`, &present); err != nil {
		t.Fatalf("present but empty: got error %v, want nil", err)
	}
}

// Slices, nested structs and pointers go through the general path and must
// still work.
func TestDirectBindCompositeKinds(t *testing.T) {
	type inner struct {
		V string `body:"v"`
	}
	var got struct {
		Tags  []string `body:"tags"`
		Nums  []int    `body:"nums"`
		Inner inner    `body:"inner"`
		Ptr   *string  `body:"ptr"`
	}

	body := `{"tags":["a","b"],"nums":[1,2],"inner":{"v":"deep"},"ptr":"pointed"}`
	if err := bindBody(t, body, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Tags) != 2 || len(got.Nums) != 2 || got.Inner.V != "deep" {
		t.Errorf("bound %+v", got)
	}
	if got.Ptr == nil || *got.Ptr != "pointed" {
		t.Errorf("Ptr = %v, want \"pointed\"", got.Ptr)
	}
}

// Members nothing binds are skipped without disturbing the rest.
func TestDirectBindSkipsUnboundMembers(t *testing.T) {
	var got struct {
		Keep string `body:"keep"`
	}
	body := `{"a":[1,{"b":null}],"keep":"kept","z":{"deep":{"deeper":[true]}}}`
	if err := bindBody(t, body, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Keep != "kept" {
		t.Errorf("Keep = %q, want %q", got.Keep, "kept")
	}
}

// The last of a repeated member wins, whichever path binds it.
func TestDirectBindDuplicateMembers(t *testing.T) {
	var got struct {
		S string `body:"s"`
		I int    `body:"i"`
	}
	if err := bindBody(t, `{"s":"first","i":1,"s":"second","i":2}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.S != "second" || got.I != 2 {
		t.Errorf("bound %+v, want the last of each", got)
	}
}

// Each fast destination must fall back correctly when the token does not
// match it, so every fast kind is exercised with a mismatched token.
func TestDirectBindFastKindFallbacks(t *testing.T) {
	var got struct {
		S string  `body:"s"` // given a bool
		I int     `body:"i"` // given a string
		U uint    `body:"u"` // given a bool-shaped string
		F float64 `body:"f"` // given a string
		B bool    `body:"b"` // given a number
	}

	if err := bindBody(t, `{"s":true,"i":"7","u":"7","f":"7.5","b":1}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.S != "true" || got.I != 7 || got.U != 7 || got.F != 7.5 || !got.B {
		t.Errorf("bound %+v, want each mismatched token converted", got)
	}
}

// An overflowing value is refused rather than silently truncated. reflect
// wraps on assignment, so 9999 into an int8 would otherwise bind as 15.
func TestOverflowIsRefused(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		target interface{}
	}{
		{"int8 from body", `{"v":9999}`, &struct {
			V int8 `body:"v"`
		}{}},
		{"uint8 from body", `{"v":9999}`, &struct {
			V uint8 `body:"v"`
		}{}},
		{"float32 from body", `{"v":1e300}`, &struct {
			V float32 `body:"v"`
		}{}},
		{"int8 from a string", `{"v":"9999"}`, &struct {
			V int8 `body:"v"`
		}{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := bindBody(t, tc.body, tc.target)
			if err == nil {
				t.Fatalf("got nil error, want a refusal")
			}
			if !strings.Contains(err.Error(), "overflow") {
				t.Errorf("error %q does not mention the overflow", err)
			}
		})
	}
}

// The same guard applies to the other sources, which convert by the same path.
func TestOverflowRefusedFromQuery(t *testing.T) {
	var got struct {
		V int8 `query:"v"`
	}
	err := Bind(httptest.NewRequest("GET", "/u?v=9999", nil), &got)
	if err == nil {
		t.Fatal("got nil error, want a refusal")
	}
	if got.V != 0 {
		t.Errorf("V = %d, want it left alone", got.V)
	}
}

// A value at the edge of the range still binds.
func TestBoundaryValuesBind(t *testing.T) {
	var got struct {
		I8  int8   `body:"i8"`
		U8  uint8  `body:"u8"`
		I64 int64  `body:"i64"`
		U64 uint64 `body:"u64"`
	}
	body := `{"i8":127,"u8":255,"i64":9223372036854775807,"u64":18446744073709551615}`
	if err := bindBody(t, body, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.I8 != 127 || got.U8 != 255 || got.I64 != math.MaxInt64 || got.U64 != math.MaxUint64 {
		t.Errorf("bound %+v, want the maxima", got)
	}

	var negative struct {
		I8 int8 `body:"v"`
	}
	if err := bindBody(t, `{"v":-128}`, &negative); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if negative.I8 != -128 {
		t.Errorf("I8 = %d, want -128", negative.I8)
	}
}

// DisallowUnknownFields applies to form bodies too, which bind through the map.
func TestDisallowUnknownFieldsForm(t *testing.T) {
	var got struct {
		Email string `body:"email"`
	}
	r := httptest.NewRequest("POST", "/u", strings.NewReader("email=a%40b.c&surprise=1"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	err := BindWithOptions(r, &got, BindOptions{DisallowUnknownFields: true})
	if !errors.Is(err, ErrUnknownField) {
		t.Fatalf("got %v, want ErrUnknownField", err)
	}
	if !strings.Contains(err.Error(), `"surprise"`) {
		t.Errorf("error %q does not name the unknown key", err)
	}
}

func TestDisallowUnknownFieldsFormAccepted(t *testing.T) {
	var got struct {
		Email string `body:"email"`
	}
	r := httptest.NewRequest("POST", "/u", strings.NewReader("email=a%40b.c"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if err := BindWithOptions(r, &got, BindOptions{DisallowUnknownFields: true}); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Email != "a@b.c" {
		t.Errorf("Email = %q, want %q", got.Email, "a@b.c")
	}
}
