package binder

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// These cover the JSON body decoder. Both build configurations must agree on
// values and on which inputs are rejected, so the assertions are on the
// sentinel rather than on decoder message text, which differs between them.

type decodeTarget struct {
	Email string   `body:"email"`
	N     int64    `body:"n"`
	F     float64  `body:"f"`
	B     bool     `body:"b"`
	Tags  []string `body:"tags"`
}

func bindJSON(t *testing.T, body string, target interface{}) error {
	t.Helper()
	r := httptest.NewRequest("POST", "/u", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return Bind(r, target)
}

// A body that is not a JSON object cannot fill a struct and is reported.
func TestNonObjectJSONBodyRejected(t *testing.T) {
	for _, body := range []string{`[1,2]`, `"a string"`, `123`, `true`, `false`} {
		var got decodeTarget
		err := bindJSON(t, body, &got)
		if !errors.Is(err, ErrMalformedBody) {
			t.Errorf("body %s: got %v, want ErrMalformedBody", body, err)
		}
	}
}

// A literal null body carries no members and is not an error.
func TestNullJSONBodyIsEmpty(t *testing.T) {
	var got struct {
		Q     string `query:"q"`
		Email string `body:"email"`
	}
	r := httptest.NewRequest("POST", "/u?q=v", strings.NewReader(`null`))
	r.Header.Set("Content-Type", "application/json")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("null body: got error %v, want nil", err)
	}
	if got.Q != "v" {
		t.Errorf("Q = %q, want %q - other sources must still bind", got.Q, "v")
	}
}

func TestEmptyJSONObject(t *testing.T) {
	var got decodeTarget
	if err := bindJSON(t, `{}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
}

// Every JSON value kind reaches the right Go type.
func TestJSONValueKinds(t *testing.T) {
	type nested struct {
		K string `body:"k"`
	}
	var got struct {
		Str   string   `body:"str"`
		Num   int64    `body:"num"`
		Float float64  `body:"float"`
		True  bool     `body:"true"`
		False bool     `body:"false"`
		Arr   []string `body:"arr"`
		Obj   nested   `body:"obj"`
	}
	got.False = true // so binding false is visibly a change

	body := `{"str":"s","num":42,"float":1.5,"true":true,"false":false,"arr":["a","b"],"obj":{"k":"v"}}`
	if err := bindJSON(t, body, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Str != "s" || got.Num != 42 || got.Float != 1.5 || !got.True || got.False {
		t.Errorf("bound %+v, want each kind converted", got)
	}
	if len(got.Arr) != 2 {
		t.Errorf("Arr = %v, want two entries", got.Arr)
	}
	if got.Obj.K != "v" {
		t.Errorf("Obj.K = %q, want %q", got.Obj.K, "v")
	}
}

// A null inside an array or object is preserved as a nil element.
func TestNullsInsideValues(t *testing.T) {
	var got struct {
		Tags []*string `body:"tags"`
	}
	if err := bindJSON(t, `{"tags":["a",null]}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Tags) != 2 {
		t.Fatalf("Tags = %v, want two entries", got.Tags)
	}
	if got.Tags[0] == nil || *got.Tags[0] != "a" {
		t.Errorf("Tags[0] = %v, want \"a\"", got.Tags[0])
	}
}

// Duplicate member names take the last value, as encoding/json does.
func TestDuplicateMemberNamesTakeTheLast(t *testing.T) {
	var got struct {
		N int64 `body:"n"`
	}
	if err := bindJSON(t, `{"n":1,"n":2}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.N != 2 {
		t.Errorf("N = %d, want 2", got.N)
	}
}

// Members no field binds are skipped, whatever they contain, and must not
// affect the members that are bound.
func TestUnboundMembersSkipped(t *testing.T) {
	var got struct {
		Email string `body:"email"`
	}
	body := `{"before":{"deep":[1,2,{"x":null}]},"email":"a@b.c","after":[[[]]],"n":9007199254740993}`
	if err := bindJSON(t, body, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Email != "a@b.c" {
		t.Errorf("Email = %q, want %q", got.Email, "a@b.c")
	}
}

// A malformed member that is skipped is still malformed: skipping validates.
func TestMalformedSkippedMemberStillReported(t *testing.T) {
	var got struct {
		Email string `body:"email"`
	}
	err := bindJSON(t, `{"unbound":[1,,2],"email":"a@b.c"}`, &got)
	if !errors.Is(err, ErrMalformedBody) {
		t.Fatalf("got %v, want ErrMalformedBody", err)
	}
}

// Truncation at various depths is reported rather than silently accepted.
func TestTruncatedJSONReported(t *testing.T) {
	for _, body := range []string{`{`, `{"a"`, `{"a":`, `{"a":1`, `{"a":[1`, `{"a":{"b"`} {
		var got decodeTarget
		if err := bindJSON(t, body, &got); !errors.Is(err, ErrMalformedBody) {
			t.Errorf("body %q: got %v, want ErrMalformedBody", body, err)
		}
	}
}

// Deeply nested values decode correctly through the recursive walk.
func TestDeepNestingDecodes(t *testing.T) {
	type level3 struct {
		V string `body:"v"`
	}
	type level2 struct {
		L3 level3 `body:"l3"`
	}
	type level1 struct {
		L2 level2 `body:"l2"`
	}
	var got struct {
		L1 level1 `body:"l1"`
	}
	if err := bindJSON(t, `{"l1":{"l2":{"l3":{"v":"deep"}}}}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.L1.L2.L3.V != "deep" {
		t.Errorf("V = %q, want %q", got.L1.L2.L3.V, "deep")
	}
}

// Arrays of objects reach slices of nested structs.
func TestArrayOfObjects(t *testing.T) {
	type item struct {
		A int64  `body:"a"`
		B string `body:"b"`
	}
	var got struct {
		Items []item `body:"items"`
	}
	if err := bindJSON(t, `{"items":[{"a":1},{"b":"two"}]}`, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("Items = %v, want two entries", got.Items)
	}
	if got.Items[0].A != 1 || got.Items[1].B != "two" {
		t.Errorf("Items = %+v, want [{A:1} {B:two}]", got.Items)
	}
}
