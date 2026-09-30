package binder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A request declaring a huge Content-Length used to have its whole length
// allocated before any of it arrived, or, with a limit near MaxInt64, panic.
func TestDeclaredLengthDoesNotPresizeBeyondCap(t *testing.T) {
	var got struct {
		S string `body:"s"`
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"s":"x"}`))
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = math.MaxInt64
	if err := BindWithOptions(r, &got, BindOptions{MaxBodySize: math.MaxInt64}); err != nil || got.S != "x" {
		t.Errorf("got %v, S=%q; want nil, x", err, got.S)
	}

	r = httptest.NewRequest("POST", "/", strings.NewReader(`{"s":"x"}`))
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = 1 << 30
	buf, err := readBody(r, 2<<30)
	if err != nil || cap(buf) > maxPresize+1 {
		t.Errorf("readBody: cap %d, err %v; want at most %d", cap(buf), err, maxPresize+1)
	}
}

// A null inside a nested struct must not allocate an embedded pointer.
func TestNestedNullDoesNotAllocateEmbeddedPointer(t *testing.T) {
	type inner struct {
		*Audit
		N int `body:"n"`
	}
	var got struct {
		Inner inner `body:"inner"`
	}
	if err := Bind(embedRequest("/", `{"inner":{"by":null,"n":1}}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Inner.Audit != nil || got.Inner.N != 1 {
		t.Errorf("got %+v, want Audit nil and N 1", got.Inner)
	}
}

var errValidated = errors.New("validated")

type ValBase struct {
	B string `query:"b"`
}

func (ValBase) Validate(context.Context) error { return errValidated }

type ValMid struct {
	ValBase
}

type viaNilPtr struct {
	*ValBase
	Y string `query:"y"`
}

type viaValue struct {
	ValBase
}

type deepViaNilPtr struct {
	*ValMid
}

type ownDespiteEmbed struct {
	*ValBase
}

func (ownDespiteEmbed) Validate(context.Context) error { return errValidated }

type ownPtrDespiteEmbed struct {
	*ValBase
}

func (*ownPtrDespiteEmbed) Validate(context.Context) error { return errValidated }

// A Validate promoted from an embedded pointer that stayed nil, because none
// of its fields was sent, used to panic dereferencing it. It is skipped; one
// that was sent, one promoted from a value, and one the type declares itself
// still run.
func TestValidatePromotedThroughNilPointer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target any
		query  string
		runs   bool
	}{
		{"promoted, pointer nil", &viaNilPtr{}, "y=1", false},
		{"promoted, pointer set", &viaNilPtr{}, "b=1", true},
		{"promoted from a value", &viaValue{}, "", true},
		{"promoted two levels down, pointer nil", &deepViaNilPtr{}, "", false},
		{"declared on value receiver", &ownDespiteEmbed{}, "", true},
		{"declared on pointer receiver", &ownPtrDespiteEmbed{}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Bind(httptest.NewRequest("GET", "/?"+tc.query, nil), tc.target)
			if ran := errors.Is(err, errValidated); ran != tc.runs {
				t.Errorf("Validate ran = %v, want %v (err %v)", ran, tc.runs, err)
			}
		})
	}
}

// Many distinct unknown members used to cost time quadratic in their number
// with DisallowUnknownFields: 80,000 took six seconds. Each is counted once,
// and the report stops at maxFailures.
func TestManyUnknownMembersStayLinear(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"s":"x"`)
	for i := range 100_000 {
		fmt.Fprintf(&b, `,"k%d":1,"k%d":2`, i, i)
	}
	b.WriteString("}")
	r := httptest.NewRequest("POST", "/", strings.NewReader(b.String()))
	r.Header.Set("Content-Type", "application/json")
	var got struct {
		S string `body:"s"`
	}
	start := time.Now()
	err := BindWithOptions(r, &got, BindOptions{MaxBodySize: -1, DisallowUnknownFields: true})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v", elapsed)
	}
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != maxFailures {
		t.Errorf("got %d failures, want %d", len(errs), maxFailures)
	}
}

// An array bound into a slice, at the top level or nested, used to pass
// through []any and a json.Number per element: an 8 MB body allocated
// 433 MB. It is decoded straight into the slice.
func TestArrayDecodesWithoutAmplification(t *testing.T) {
	const n = 1 << 20
	body := `{"s":[` + strings.Repeat("0,", n-1) + `0],"inner":{"s":[` + strings.Repeat("0,", n-1) + `0]}}`
	var got struct {
		S     []int8 `body:"s"`
		Inner struct {
			S []int8 `body:"s"`
		} `body:"inner"`
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := BindWithOptions(r, &got, BindOptions{MaxBodySize: -1}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if len(got.S) != n || len(got.Inner.S) != n {
		t.Fatalf("bound %d and %d elements, want %d", len(got.S), len(got.Inner.S), n)
	}
	// The body is 4 MB; the slices, grown by doubling, are about 2 MB each.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 32<<20 {
		t.Errorf("allocated %d MB for a %d MB body", alloc>>20, len(body)>>20)
	}
}

type CycleA struct {
	*CycleB
	N int `query:"n"`
}

type CycleB struct {
	*CycleA
	Validator
}

// valInt supplies Validate from a type that is not a struct, which the search
// for where Validate comes from does not descend into.
type valInt int

func (valInt) Validate(context.Context) error { return errValidated }

type CycleC struct {
	*CycleD
	valInt
	N int `query:"n"`
}

type CycleD struct {
	*CycleC
}

// Embedded pointers that refer to each other used to hang the search for
// where Validate comes from, holding the type cache's lock, so every later
// Bind hung too. The search must end whether or not it finds Validate on a
// struct: CycleC's comes from valInt, so only the cycle guard stops it.
func TestValidateSearchTerminatesOnCycle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target any
		want   error
	}{
		{"nil embedded Validator is skipped", &CycleA{}, nil},
		{"Validate from a non-struct runs", &CycleC{}, errValidated},
	} {
		done := make(chan error, 1)
		go func() { done <- Bind(httptest.NewRequest("GET", "/?n=1", nil), tc.target) }()
		select {
		case err := <-done:
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: Bind did not return", tc.name)
		}
	}
}

type validatorEmbed struct {
	Validator
	N int `query:"n"`
}

type validatorFunc func(context.Context) error

func (f validatorFunc) Validate(ctx context.Context) error { return f(ctx) }

// A nil embedded Validator used to panic when called. It is skipped; one that
// holds a value runs.
func TestEmbeddedValidatorInterface(t *testing.T) {
	var got validatorEmbed
	if err := Bind(httptest.NewRequest("GET", "/?n=1", nil), &got); err != nil {
		t.Errorf("nil: got %v, want nil", err)
	}
	got = validatorEmbed{Validator: validatorFunc(func(context.Context) error { return errValidated })}
	if err := Bind(httptest.NewRequest("GET", "/?n=1", nil), &got); !errors.Is(err, errValidated) {
		t.Errorf("set: got %v, want errValidated", err)
	}
}

// A null element of a pointer slice used to be allocated as a pointer to
// zero. It stays nil, as in encoding/json.
func TestNullElementStaysNil(t *testing.T) {
	var got struct {
		P []*int `body:"p"`
	}
	if err := Bind(embedRequest("/", `{"p":[null,1]}`), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.P) != 2 || got.P[0] != nil || got.P[1] == nil || *got.P[1] != 1 {
		t.Errorf("P = %v, want [nil &1]", got.P)
	}
}

// A promoted omitempty field sent empty at the top level of a JSON body used
// to allocate its embedded pointer; a form body did not. Neither does now.
func TestEmptyPromotedOmitEmptyAllocatesNothing(t *testing.T) {
	type Audit2 struct {
		By string `body:"by,omitempty"`
	}
	var target struct {
		*Audit2
	}
	if err := Bind(embedRequest("/", `{"by":""}`), &target); err != nil {
		t.Fatal(err)
	}
	if target.Audit2 != nil {
		t.Errorf("Audit2 = %+v, want nil", target.Audit2)
	}
}

// A request built without a URL used to panic on a query field.
func TestNilURLHasNoQuery(t *testing.T) {
	r := httptest.NewRequest("GET", "/?q=1", nil)
	r.URL = nil
	var got struct {
		Q string `query:"q"`
	}
	if err := Bind(r, &got); err != nil || got.Q != "" {
		t.Errorf("got %v, Q=%q; want nil, empty", err, got.Q)
	}
}

type deepNode struct {
	C []deepNode `json:"c,omitempty"`
	N string     `json:"n,omitempty"`
}

// omitempty on a nested array used to read the value, copy it and decode it
// again, level by level, so a deep value cost time and memory quadratic in its
// depth: 5,000 levels allocated 388 MB. Each byte is now read once.
func TestDeepOmitEmptyIsLinear(t *testing.T) {
	const depth = 4000
	body := `{"c":` + strings.Repeat(`[{"c":`, depth) + `[]` + strings.Repeat(`}]`, depth) + `}`
	var got struct {
		C []deepNode `json:"c,omitempty"`
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := bindJSONBody(t, body, &got); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 20<<20 {
		t.Errorf("allocated %d MB for a %d KB body", alloc>>20, len(body)>>10)
	}
	n := 0
	for c := got.C; len(c) > 0; c = c[0].C {
		n++
	}
	if n != depth {
		t.Errorf("depth %d, want %d", n, depth)
	}
}

// A member sent twice binds its last occurrence, but a failure in an earlier
// occurrence stands, as in encoding/json, wherever the member is.
func TestDuplicateMemberFailureStands(t *testing.T) {
	var got struct {
		Age int `json:"age"`
		In  struct {
			Age int `json:"age"`
		} `json:"in"`
		M map[string]int `json:"m"`
	}
	err := bindJSONBody(t, `{"age":"x","age":5,"in":{"age":"x","age":6},"m":{"a":"x","a":7}}`, &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 3 {
		t.Fatalf("got %v, want three failures", err)
	}
	// A later occurrence that fails too neither replaces the first failure
	// nor adds a second.
	err = bindJSONBody(t, `{"age":"x","age":"y","in":{"age":"x","age":"y"},"m":{"a":"x","a":"y"}}`, &got)
	if !errors.As(err, &errs) || len(errs) != 3 {
		t.Fatalf("got %v, want three failures", err)
	}
	for _, e := range errs {
		if !strings.Contains(e.Error(), `"x"`) {
			t.Errorf("%s: failure %v, want the first occurrence's", e.Name, e)
		}
	}
	var good struct {
		Age int `json:"age"`
	}
	if err := bindJSONBody(t, `{"age":1,"age":5}`, &good); err != nil || good.Age != 5 {
		t.Errorf("good repeat: got %v, Age=%d; want nil, 5", err, good.Age)
	}
}

type mapAddr struct {
	Zip  string `body:"zip,required"`
	City int    `body:"city"`
}

// Failures within a map come in key order, numeric for integer keys, and an
// entry's own failures in the order its fields are declared.
func TestMapFailureOrder(t *testing.T) {
	var got struct {
		M map[string]mapAddr `json:"m"`
		N map[int]int        `json:"n"`
	}
	err := bindJSONBody(t, `{"m":{"b":{"city":"x"},"a":{"city":"y"}},"n":{"10":"x","9":"y"}}`, &got)
	var errs BindErrors
	if !errors.As(err, &errs) {
		t.Fatalf("got %v", err)
	}
	var names []string
	for _, e := range errs {
		names = append(names, e.Name)
	}
	want := []string{"m[a].zip", "m[a].city", "m[b].zip", "m[b].city", "n[9]", "n[10]"}
	if !slices.Equal(names, want) {
		t.Errorf("order %v, want %v", names, want)
	}
}

// A type that decodes itself is handed the bytes as sent with omitempty too,
// at the top level as when nested.
func TestOmitEmptyRawMessageKeptAsSent(t *testing.T) {
	var got struct {
		Raw json.RawMessage `json:"raw,omitempty"`
		In  struct {
			Raw json.RawMessage `json:"raw,omitempty"`
		} `json:"in"`
	}
	const raw = `{"b":1,"a":"é<"}`
	if err := bindJSONBody(t, `{"raw":`+raw+`,"in":{"raw":`+raw+`}}`, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Raw) != raw || string(got.In.Raw) != raw {
		t.Errorf("got %s and %s, want %s", got.Raw, got.In.Raw, raw)
	}
}

// A JSON string into a []byte is base64, as encoding/json encodes one.
func TestBase64IntoBytes(t *testing.T) {
	var got struct {
		B  []byte `json:"b"`
		In struct {
			B []byte `json:"b"`
		} `json:"in"`
		L [][]byte `json:"l"`
	}
	if err := bindJSONBody(t, `{"b":"aGk=","in":{"b":"aGk="},"l":["aGk="]}`, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.B) != "hi" || string(got.In.B) != "hi" || len(got.L) != 1 || string(got.L[0]) != "hi" {
		t.Errorf("got %q %q %q", got.B, got.In.B, got.L)
	}
	if err := bindJSONBody(t, `{"b":"!!"}`, &got); err == nil {
		t.Error("invalid base64 bound, want an error")
	}
}

// wideElem is large, so a buffer allocated for an empty array of them shows.
type wideElem struct {
	N int `json:"n"`
	_ [64]int64
}

// An empty inner array costs a small, fixed amount rather than a buffer.
func TestEmptyInnerArraysAreCheap(t *testing.T) {
	body := `{"s":[` + strings.Repeat("[],", 9999) + `[]]}`
	var got struct {
		S [][]wideElem `json:"s"`
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := bindJSONBody(t, body, &got); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if per := (after.TotalAlloc - before.TotalAlloc) / 10000; per > 200 {
		t.Errorf("%d bytes per empty array", per)
	}
}

type failTree struct {
	K []failTree `json:"k"`
	W []int      `json:"w"`
}

// A failure deep in a body used to be rebuilt, path and all, at every level on
// the way out, so depth multiplied its cost: 5,000 failures 1,000 levels down
// took twelve seconds and 83 GB. Paths are now joined once, and at most
// maxFailures failures are built.
func TestDeepFailuresAreLinear(t *testing.T) {
	const depth = 1000
	// With "w" first, the budget is spent on the outermost levels; with "k"
	// first, on the deepest, whose paths are a thousand levels long.
	for _, wFirst := range []bool{true, false} {
		var b strings.Builder
		for range depth {
			if wFirst {
				b.WriteString(`{"w":["x","x","x","x","x"],"k":[`)
			} else {
				b.WriteString(`{"k":[`)
			}
		}
		b.WriteString(`{}`)
		for range depth {
			if wFirst {
				b.WriteString(`]}`)
			} else {
				b.WriteString(`],"w":["x","x","x","x","x"]}`)
			}
		}
		body := `{"k":[` + b.String() + `]}`
		var got struct {
			K []failTree `json:"k"`
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		err := bindJSONBody(t, body, &got)
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)

		var errs BindErrors
		if !errors.As(err, &errs) || len(errs) != maxFailures {
			t.Fatalf("wFirst=%v: got %d failures, want %d", wFirst, len(errs), maxFailures)
		}
		last := errs[len(errs)-1]
		if wFirst {
			// Failures come in field order, and K is declared before W, so
			// the deepest come first and the outermost level's last.
			if last.Name != "k[0].w[4]" || last.Field != "K[0].W[4]" {
				t.Errorf("last failure named %q / %q", last.Name, last.Field)
			}
		} else if !strings.HasPrefix(last.Name, "k[0].k[0]") || !strings.HasSuffix(last.Name, "k[0].w[4]") || !strings.Contains(last.Name, "…") || len(last.Name) > 400 {
			t.Errorf("last failure named %.200q", last.Name)
		}
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 || elapsed > 2*time.Second {
			t.Errorf("wFirst=%v: allocated %d MB in %v for a %d KB body", wFirst, alloc>>20, elapsed, len(body)>>10)
		}
	}
}

// One failure at the bottom of a deep body is named by its path, the middle
// elided past maxPathSegments, at a cost linear in its depth.
func TestSingleDeepFailure(t *testing.T) {
	const depth = 4000
	body := `{"k":[` + strings.Repeat(`{"k":[`, depth) + `{"w":["x"]}` + strings.Repeat(`]}`, depth) + `]}`
	var got struct {
		K []failTree `json:"k"`
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := bindJSONBody(t, body, &got)
	runtime.ReadMemStats(&after)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 1 {
		t.Fatalf("got %v", err)
	}
	// A path this deep keeps its ends and elides the middle.
	name := errs[0].Name
	if !strings.HasPrefix(name, "k[0].k[0].k[0]") || !strings.HasSuffix(name, "k[0].k[0].w[0]") || !strings.Contains(name, "…") || len(name) > 400 {
		t.Errorf("name %q", name)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Errorf("allocated %d MB", alloc>>20)
	}
}

// A flood of bad values reports maxFailures of them and stops decoding the
// rest: a million used to allocate 729 MB and report every one.
func TestFailureFloodIsCheap(t *testing.T) {
	body := `{"n":[` + strings.Repeat(`"x",`, 999_999) + `"x"]}`
	var got struct {
		N []int `json:"n"`
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := BindWithOptions(regressJSONRequest(body), &got, BindOptions{MaxBodySize: -1})
	runtime.ReadMemStats(&after)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != maxFailures {
		t.Fatalf("got %d failures, want %d", len(errs), maxFailures)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > uint64(3*len(body)) {
		t.Errorf("allocated %d MB for a %d MB body", alloc>>20, len(body)>>20)
	}
}

// A value of the wrong shape is refused without being decoded: an array sent
// for a string used to be decoded whole first, at 57 times its size.
func TestMismatchedShapeIsSkipped(t *testing.T) {
	body := `{"name":[` + strings.Repeat("1,", 499_999) + `1],"rows":[` + strings.Repeat("[1],", 99_999) + `[1]]}`
	var got struct {
		Name string            `json:"name"`
		Rows []struct{ A int } `json:"rows"`
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := BindWithOptions(regressJSONRequest(body), &got, BindOptions{MaxBodySize: -1})
	runtime.ReadMemStats(&after)
	var errs BindErrors
	if !errors.As(err, &errs) || errs[0].Name != "name" || !strings.Contains(errs[0].Error(), "JSON array") {
		t.Fatalf("got %v", err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > uint64(4*len(body)) {
		t.Errorf("allocated %d MB for a %d MB body", alloc>>20, len(body)>>20)
	}
}

type SelfPointer *SelfPointer

// A pointer type that points to itself used to hang binding forever, holding
// the type cache's lock. It is refused as an invalid target.
func TestSelfPointerIsInvalidTarget(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		var got struct {
			P SelfPointer `json:"p"`
			N int         `query:"n"`
		}
		done <- Bind(httptest.NewRequest("GET", "/?n=1", nil), &got)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("got %v, want ErrInvalidTarget", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Bind did not return")
	}
}

type PromotedP struct {
	P int `json:"p"`
}

// A promoted member sent twice, the first time with a bad value, keeps that
// failure even when the last occurrence is null.
func TestPromotedDuplicateNull(t *testing.T) {
	var got struct {
		PromotedP
	}
	if err := bindJSONBody(t, `{"p":"bad","p":null}`, &got); err == nil {
		t.Error("got nil, want the failure to stand")
	}
}

// Map failures come in the same order every time, whatever mix of keys
// failed, and two spellings of one key are one entry.
func TestMapKeysOrderedAndCanonical(t *testing.T) {
	body := `{"m":{"9":"x","10":"x","1a":"x","2":"x","1b":"x","30":"x"}}`
	var first []string
	for range 50 {
		var got struct {
			M map[int]int `json:"m"`
		}
		var errs BindErrors
		errors.As(bindJSONBody(t, body, &got), &errs)
		var names []string
		for _, e := range errs {
			names = append(names, e.Name)
		}
		if first == nil {
			first = names
		} else if !slices.Equal(names, first) {
			t.Fatalf("order %v, then %v", first, names)
		}
	}
	if want := []string{"m[2]", "m[9]", "m[10]", "m[30]", "m[1a]", "m[1b]"}; !slices.Equal(first, want) {
		t.Errorf("order %v, want %v", first, want)
	}

	// Two spellings of one key are one entry, and a failure in either
	// stands, reported once.
	var got struct {
		M map[int]int `json:"m"`
	}
	var errs BindErrors
	for _, body := range []string{`{"m":{"01":"x","1":5}}`, `{"m":{"01":"x","1":"y"}}`, `{"m":{"1":"x","01":"y"}}`} {
		if err := bindJSONBody(t, body, &got); !errors.As(err, &errs) || len(errs) != 1 || !strings.Contains(errs[0].Error(), `"x"`) {
			t.Errorf("%s: got %v, want one failure, for x", body, err)
		}
	}
}

type flag uint8

func (f *flag) UnmarshalText(b []byte) error {
	if string(b) != "on" {
		return fmt.Errorf("not a flag: %s", b)
	}
	*f = 1
	return nil
}

// Base64 applies to a plain []byte only; a byte type that unmarshals itself
// takes a JSON string as a one-element slice, as from a query. From a query
// or form, a []byte is the text's bytes.
func TestByteSliceRules(t *testing.T) {
	var got struct {
		Flags []flag `json:"flags"`
		Q     []byte `query:"q"`
	}
	r := regressJSONRequest(`{"flags":"on"}`)
	r.URL.RawQuery = "q=abc"
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Flags) != 1 || got.Flags[0] != 1 || string(got.Q) != "abc" {
		t.Errorf("got Flags=%v Q=%q", got.Flags, got.Q)
	}
}

func regressJSONRequest(body string) *http.Request {
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

type RecursiveList []RecursiveList

type RecursivePtrList []*RecursivePtrList

// A single value into a recursive list type used to wrap itself as a
// one-element list forever, until the stack overflowed and killed the
// process. A single value is not a list of lists.
func TestSingleValueIntoRecursiveList(t *testing.T) {
	var q struct {
		P RecursiveList    `query:"p"`
		R RecursivePtrList `query:"r"`
	}
	var errs BindErrors
	if err := Bind(httptest.NewRequest("GET", "/?p=x&r=y", nil), &q); !errors.As(err, &errs) || len(errs) != 2 {
		t.Errorf("query: got %v, want two failures", err)
	}
	var j struct {
		P RecursiveList `json:"p"`
	}
	if err := bindJSONBody(t, `{"p":"x"}`, &j); !errors.As(err, &errs) {
		t.Errorf("json: got %v, want a failure", err)
	}
	// A list of lists from nested arrays still binds.
	if err := bindJSONBody(t, `{"p":[[],[[]]]}`, &j); err != nil || len(j.P) != 2 || len(j.P[1]) != 1 {
		t.Errorf("nested arrays: got %v, %v", err, j.P)
	}
}

// Failures grouped by a conversion, such as each "x" given for a []int, used
// to escape the failure budget: 400 KB allocated 77 MB.
func TestGroupedFailuresCountAgainstBudget(t *testing.T) {
	body := `{"n":[` + strings.Repeat(`"x",`, 99_999) + `"x"],"m":{` + strings.Repeat(`"k":"x",`, 9_999) + `"k":"x"}}`
	var got struct {
		N [][]int          `json:"n"`
		M map[string][]int `json:"m"`
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := BindWithOptions(regressJSONRequest(body), &got, BindOptions{MaxBodySize: -1})
	runtime.ReadMemStats(&after)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != maxFailures {
		t.Fatalf("got %d failures, want %d", len(errs), maxFailures)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > uint64(4*len(body)) {
		t.Errorf("allocated %d KB for a %d KB body", alloc>>10, len(body)>>10)
	}
}

// A hundred failures thousands of levels down used to cost the depth times
// the failures: 170 MB for a 54 KB body. Paths past maxPathSegments are
// elided in the middle.
func TestManyDeepFailuresAreBounded(t *testing.T) {
	// Each level nests twice, an object and an array, so 4,900 levels
	// approach jsontext's limit of 10,000.
	const depth = 4900
	body := `{"k":[` + strings.Repeat(`{"k":[`, depth) + `{"w":[` + strings.Repeat(`"x",`, 99) + `"x"]}` + strings.Repeat(`]}`, depth) + `]}`
	var got struct {
		K []failTree `json:"k"`
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	err := bindJSONBody(t, body, &got)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != maxFailures {
		t.Fatalf("got %d failures: %.200v", len(errs), err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 24<<20 || elapsed > time.Second {
		t.Errorf("allocated %d MB in %v for a %d KB body", alloc>>20, elapsed, len(body)>>10)
	}
}

// Once the budget was spent, a member sent again cleared its earlier
// failures and, its value skipped, bound nothing: the bind reported success,
// even past a required field. The failures now stand.
func TestRepeatAfterBudgetStillFails(t *testing.T) {
	bad := `[` + strings.Repeat(`"x",`, maxFailures-1) + `"x"]`
	type addr struct {
		X    []int  `body:"x"`
		City string `body:"city,required"`
	}
	for _, tc := range []struct {
		body   string
		target any
	}{
		{`{"tags":` + bad + `,"tags":[1,2,3]}`, &struct {
			Tags []int `json:"tags"`
		}{}},
		{`{"addr":{"x":` + bad + `},"addr":{}}`, &struct {
			Addr addr `json:"addr"`
		}{}},
		{`{"m":{"a":` + bad + `,"a":[7]}}`, &struct {
			M map[string][]int `json:"m"`
		}{}},
		{`{"s":[{"x":` + bad + `,"x":[]},{"x":[1]},{"x":[2]}]}`, &struct {
			S []struct {
				X []int `json:"x"`
			} `json:"s"`
		}{}},
	} {
		if err := bindJSONBody(t, tc.body, tc.target); err == nil {
			t.Errorf("%.40s…: got nil, want the failures", tc.body)
		}
	}
}

type byteAlias []byte

// Base64 applies to a []byte behind pointers too.
func TestBase64BehindPointers(t *testing.T) {
	var got struct {
		P *[]byte            `json:"p"`
		M map[string]*[]byte `json:"m"`
		S []*[]byte          `json:"s"`
		A *byteAlias         `json:"a"`
	}
	if err := bindJSONBody(t, `{"p":"aGk=","m":{"k":"aGk="},"s":["aGk="],"a":"aGk="}`, &got); err != nil {
		t.Fatal(err)
	}
	if string(*got.P) != "hi" || string(*got.M["k"]) != "hi" || string(*got.S[0]) != "hi" || string(*got.A) != "hi" {
		t.Errorf("got %q %q %q %q", *got.P, *got.M["k"], *got.S[0], *got.A)
	}
}

// looseKey unmarshals any text but prints the same for all of it.
type looseKey struct{ s string }

func (k *looseKey) UnmarshalText(b []byte) error { k.s = string(b); return nil }
func (k looseKey) String() string                { return "K" }

// Failures were keyed by a key's printed form, so a later, different key that
// printed the same was taken for the earlier one: its failure deleted the
// earlier one's, or it was skipped as a repeat of it.
func TestMapFailureKeyedByValue(t *testing.T) {
	var got struct {
		M map[looseKey]int `json:"m"`
	}
	if err := bindJSONBody(t, `{"m":{"a":"x","b":1}}`, &got); err == nil {
		t.Error("got nil, want the failure for a")
	}
	// Two different keys that print the same each report their own failure.
	var errs BindErrors
	err := bindJSONBody(t, `{"m":{"a":"x","b":"y"}}`, &got)
	if !errors.As(err, &errs) {
		t.Fatalf("got %v", err)
	}
	var names []string
	for _, e := range errs {
		names = append(names, e.Name)
	}
	if want := []string{"m[a]", "m[b]"}; !slices.Equal(names, want) {
		t.Errorf("JSON: got %v, want %v", names, want)
	}
	// As from a query.
	var q struct {
		M map[looseKey]int `query:"m"`
	}
	errs = nil
	if err := Bind(httptest.NewRequest("GET", "/?m[a]=x&m[b]=y", nil), &q); !errors.As(err, &errs) || len(errs) != 2 {
		t.Errorf("query: got %v, want failures for a and b", err)
	}
}

// failCount counts conversions attempted, each of which fails.
var failCount atomic.Int64

type countedFail struct{}

func (*countedFail) UnmarshalText([]byte) error {
	failCount.Add(1)
	return errors.New("bad")
}

// A query or form map, or slice, stops converting once maxFailures entries
// have failed.
func TestTextSourcesStopAtCap(t *testing.T) {
	var b strings.Builder
	// Within url.ParseQuery's limit of 10,000 parameters.
	for i := range 4000 {
		fmt.Fprintf(&b, "m[k%d]=x&n=x&", i)
	}
	for _, target := range []any{
		&struct {
			M map[string]countedFail `body:"m"`
		}{},
		&struct {
			N []countedFail `body:"n"`
		}{},
	} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(b.String()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		failCount.Store(0)
		var errs BindErrors
		if err := Bind(r, target); !errors.As(err, &errs) || len(errs) != maxFailures {
			t.Errorf("%T: got %d failures, want %d: %.200v", target, len(errs), maxFailures, err)
		}
		if n := failCount.Load(); n > maxFailures {
			t.Errorf("%T: converted %d values, want at most %d", target, n, maxFailures)
		}
	}
}

// Top-level failures are charged to the budget like any other, so a nested
// value after them stops converting once the budget is spent.
func TestTopLevelFailureIsCharged(t *testing.T) {
	var got struct {
		A int           `json:"a"`
		N []countedFail `json:"n"`
	}
	body := `{"a":"x","n":[` + strings.Repeat(`"x",`, maxFailures-1) + `"x"]}`
	failCount.Store(0)
	if err := bindJSONBody(t, body, &got); err == nil {
		t.Fatal("got nil")
	}
	if n := failCount.Load(); n != maxFailures-1 {
		t.Errorf("converted %d elements, want %d", n, maxFailures-1)
	}
}

// An array or object sent for an interface with methods is refused without
// being decoded, and an unexported field of a pointer-cycle type that nothing
// binds does not make the target invalid.
func TestInterfaceMismatchAndUnboundCycle(t *testing.T) {
	var got struct {
		S fmt.Stringer `json:"s"`
		p SelfPointer
	}
	_ = got.p
	var errs BindErrors
	if err := bindJSONBody(t, `{"s":[1,2]}`, &got); !errors.As(err, &errs) || !strings.Contains(errs[0].Error(), "JSON array") {
		t.Errorf("got %v", err)
	}
}

// A bad top-level member sent over and over was converted and its failure
// built every time: a 10 MB body allocated 722 MB. Top-level failures are
// charged to the budget like any other.
func TestRepeatedTopLevelFailureIsCheap(t *testing.T) {
	body := `{` + strings.Repeat(`"t":"x",`, 199_999) + `"t":"x"}`
	var got struct {
		T []int `json:"t"`
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := BindWithOptions(regressJSONRequest(body), &got, BindOptions{MaxBodySize: -1})
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("got nil")
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > uint64(4*len(body)) {
		t.Errorf("allocated %d MB for a %d MB body", alloc>>20, len(body)>>20)
	}
}

type deepN struct {
	K []deepN `json:"k"`
	V int     `json:"v"`
}

// A deep value's failures were copied at every level on the way out, 405
// times the body. They are passed up as one list.
func TestDeepFailureListNotCopiedPerLevel(t *testing.T) {
	const depth = 4900
	body := `{"k":[` + strings.Repeat(`{"k":[`, depth) + strings.Repeat(`{"v":"x"},`, 99) + `{"v":"x"}` + strings.Repeat(`]}`, depth) + `]}`
	var got struct {
		K []deepN `json:"k"`
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := bindJSONBody(t, body, &got)
	runtime.ReadMemStats(&after)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != maxFailures {
		t.Fatalf("got %d failures", len(errs))
	}
	// Measured against binding the same depth with good values, which pays
	// the parser's own cost of deep nesting and of the values themselves.
	good := strings.ReplaceAll(body, `"x"`, `1`)
	var ref struct {
		K []deepN `json:"k"`
	}
	var refBefore, refAfter runtime.MemStats
	runtime.ReadMemStats(&refBefore)
	if err := bindJSONBody(t, good, &ref); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&refAfter)
	alloc, refAlloc := after.TotalAlloc-before.TotalAlloc, refAfter.TotalAlloc-refBefore.TotalAlloc
	if alloc > 4*refAlloc {
		t.Errorf("allocated %d KB against %d KB for good values", alloc>>10, refAlloc>>10)
	}
	// Names and indexes stay together either side of the elision.
	name := errs[0].Name
	if strings.Contains(name, "k.…") || strings.Contains(name, "….[") || !strings.Contains(name, "].….k[") {
		t.Errorf("elided name %q", name)
	}
}

// A single value fills a list of lists as a list of one list of one, from a
// form as from a query, and a JSON string into a [][]byte element is base64.
func TestSingleValueIntoListOfLists(t *testing.T) {
	var got struct {
		F [][]string `body:"f"`
		P *[][]int   `body:"p"`
		Q [][]string `query:"q"`
	}
	r := httptest.NewRequest("POST", "/?q=z", strings.NewReader("f=x&p=5"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.F, [][]string{{"x"}}) || !reflect.DeepEqual(*got.P, [][]int{{5}}) || !reflect.DeepEqual(got.Q, [][]string{{"z"}}) {
		t.Errorf("got %v %v %v", got.F, *got.P, got.Q)
	}
	var j struct {
		B [][]byte `json:"b"`
	}
	if err := bindJSONBody(t, `{"b":"aGk="}`, &j); err != nil || len(j.B) != 1 || string(j.B[0]) != "hi" {
		t.Errorf("got %v, %q", err, j.B)
	}
}

// A repeated member whose first occurrence failed is skipped, not decoded
// again, so it adds no failures and costs no budget.
func TestRepeatOfFailedMemberIsSkipped(t *testing.T) {
	bad := func(n int) string { return `[` + strings.Repeat(`"x",`, n-1) + `"x"]` }
	var got struct {
		A []int `json:"a"`
		B []int `json:"b"`
	}
	err := bindJSONBody(t, `{"a":`+bad(3)+`,"a":`+bad(50)+`,"b":`+bad(2)+`}`, &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 5 {
		t.Errorf("got %d failures, want 5 (a's first three, b's two)", len(errs))
	}
}

// sliceKey unmarshals into a value no map can hold.
type sliceKey struct{ V any }

func (k *sliceKey) UnmarshalText(b []byte) error { k.V = bytes.Clone(b); return nil }

// A key type whose value cannot be hashed used to panic; it is an invalid key.
func TestUnhashableMapKeyIsAnError(t *testing.T) {
	var got struct {
		M map[sliceKey]int `json:"m"`
	}
	if err := bindJSONBody(t, `{"m":{"a":1}}`, &got); err == nil {
		t.Error("got nil, want an invalid-key failure")
	}
	r := httptest.NewRequest("GET", "/?m[a]=1", nil)
	var q struct {
		M map[sliceKey]int `query:"m"`
	}
	if err := Bind(r, &q); err == nil {
		t.Error("query: got nil, want an invalid-key failure")
	}
}

// Whole-number keys of any length order by value, and a nested required
// failure reads as one.
func TestLongKeyOrderAndRequiredMessage(t *testing.T) {
	var got struct {
		M map[uint8]int `json:"m"`
		N struct {
			A int `body:"a,required"`
		} `json:"n"`
	}
	// By value, 2×10^399 comes before 10^400, which its text does not.
	shorter := "2" + strings.Repeat("0", 399)
	longer := "1" + strings.Repeat("0", 400)
	err := bindJSONBody(t, `{"m":{"`+longer+`":1,"12":"x","`+shorter+`":1,"x":1},"n":{}}`, &got)
	var errs BindErrors
	if !errors.As(err, &errs) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range errs {
		names = append(names, e.Name)
	}
	// A long key is ordered by its full value, and named by its first
	// maxKeyInPath bytes.
	want := []string{"m[12]", "m[" + shorter[:maxKeyInPath] + "…]", "m[" + longer[:maxKeyInPath] + "…]", "m[x]", "n.a"}
	if !slices.Equal(names, want) {
		t.Errorf("order %.200v", names)
	}
	if last := errs[len(errs)-1]; !strings.HasPrefix(last.Message, "missing required field N.A") {
		t.Errorf("message %q", last.Message)
	}
}

type bracketNode struct {
	Next *bracketNode `json:"[n"`
	V    int          `json:"v"`
}

// A tag name beginning with a bracket was taken for an index when building a
// failure's path, which put its Go and client paths out of step and, deep
// enough, panicked. What each part is is recorded now, not guessed.
func TestBracketTagNameInPath(t *testing.T) {
	body := strings.Repeat(`{"[n":`, 80) + `{"v":"x"}` + strings.Repeat(`}`, 80)
	var got struct {
		N bracketNode `json:"n"`
	}
	var errs BindErrors
	if err := bindJSONBody(t, `{"n":`+body+`}`, &got); !errors.As(err, &errs) || len(errs) != 1 {
		t.Fatalf("got %v", err)
	}
	name, field := errs[0].Name, errs[0].Field
	if !strings.HasPrefix(name, "n.[n.[n") || !strings.HasSuffix(name, "[n.v") || !strings.HasPrefix(field, "N.Next.Next") || !strings.HasSuffix(field, "Next.V") {
		t.Errorf("got %q / %q", name, field)
	}
}

// A long run of indexes, as in a list of lists of lists, used to be folded
// across the elided gap, dropping levels without the "…" that marks them.
func TestLongIndexRunKeepsElisionMark(t *testing.T) {
	var got struct {
		Grid RecursiveList `json:"grid"`
	}
	body := `{"grid":` + strings.Repeat(`[`, 100) + `"x"` + strings.Repeat(`]`, 100) + `}`
	var errs BindErrors
	if err := bindJSONBody(t, body, &got); !errors.As(err, &errs) {
		t.Fatalf("got %v", err)
	}
	if f := errs[0].Field; !strings.Contains(f, "…") || !strings.HasPrefix(f, "Grid[0]") {
		t.Errorf("field %q", f)
	}
}

// wideRow is tagged so that a nested object binds its fields; untagged, none
// would bind, and bookkeeping sized to them would cost nothing to find.
type wideRow struct {
	A  bool `json:"a"`
	B  bool `json:"b"`
	C  bool `json:"c"`
	D  bool `json:"d"`
	E  bool `json:"e"`
	F  bool `json:"f"`
	G  bool `json:"g"`
	H  bool `json:"h"`
	I  bool `json:"i"`
	J  bool `json:"j"`
	K  bool `json:"k"`
	L  bool `json:"l"`
	M  bool `json:"m"`
	N  bool `json:"n"`
	O  bool `json:"o"`
	P  bool `json:"p"`
	Q  bool `json:"q"`
	R  bool `json:"r"`
	S  bool `json:"s"`
	T  bool `json:"t"`
	U  bool `json:"u"`
	V  bool `json:"v"`
	W  bool `json:"w"`
	X  bool `json:"x"`
	Y  bool `json:"y"`
	Z  bool `json:"z"`
	AA bool `json:"aa"`
	BB bool `json:"bb"`
	CC bool `json:"cc"`
	DD bool `json:"dd"`
	EE bool `json:"ee"`
	FF bool `json:"ff"`
}

// Every nested object allocated bookkeeping sized to its struct, failures or
// not: 290 MB for 900 KB of empty objects into a 32-field struct. A clean
// object now allocates none.
func TestCleanNestedObjectsAllocateNothingExtra(t *testing.T) {
	body := `{"w":[` + strings.Repeat(`{},`, 99_999) + `{}]}`
	var got struct {
		W []wideRow `json:"w"`
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := BindWithOptions(regressJSONRequest(body), &got, BindOptions{MaxBodySize: -1}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	// The slice itself, grown by doubling, is 32 bytes a row, 3.2 MB here.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Errorf("allocated %d MB for a %d KB body", alloc>>20, len(body)>>10)
	}
}

// omitempty leaves a field alone for an empty array or object, whatever its
// type, rather than refusing it as the wrong shape.
func TestOmitEmptyEmptyCompositeForScalar(t *testing.T) {
	var got struct {
		N  int    `json:"n,omitempty"`
		S  string `json:"s,omitempty"`
		In struct {
			N int `json:"n,omitempty"`
		} `json:"in"`
	}
	got.N, got.S, got.In.N = 7, "keep", 8
	if err := bindJSONBody(t, `{"n":[],"s":{},"in":{"n":[ ]}}`, &got); err != nil {
		t.Fatal(err)
	}
	if got.N != 7 || got.S != "keep" || got.In.N != 8 {
		t.Errorf("got %+v", got)
	}
	var errs BindErrors
	if err := bindJSONBody(t, `{"n":[1]}`, &got); !errors.As(err, &errs) || errs[0].Name != "n" {
		t.Errorf("non-empty array: got %v", err)
	}
}

// A long map key was repeated in full in the path of every failure beneath
// it: a 1 MB key over 100 failures held 400 MB and made a 210 MB message.
// A path carries at most maxKeyInPath bytes of a key.
func TestLongMapKeyIsShortenedInPaths(t *testing.T) {
	key := strings.Repeat("\u0080", 500_000)
	body := `{"m":{"` + key + `":[` + strings.Repeat(`{},`, 99) + `{}]}}`
	var got struct {
		M map[string][]struct {
			A int `body:"a,required"`
		} `json:"m"`
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := BindWithOptions(regressJSONRequest(body), &got, BindOptions{MaxBodySize: -1})
	runtime.ReadMemStats(&after)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != maxFailures {
		t.Fatalf("got %d failures", len(errs))
	}
	if n := len(err.Error()); n > 100_000 {
		t.Errorf("message is %d bytes", n)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > uint64(8*len(body)) {
		t.Errorf("allocated %d MB for a %d MB body", alloc>>20, len(body)>>20)
	}
}

// A map field whose name has a bracket in it binds its form entries, and
// they are not also reported as unknown.
func TestBracketMapNameEntriesAreKnown(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader("[b[k]=1&a[b[z]=2"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var got struct {
		Br map[string]int `body:"[b"`
		Ab map[string]int `body:"a[b"`
	}
	if err := BindWithOptions(r, &got, BindOptions{DisallowUnknownFields: true}); err != nil {
		t.Fatalf("got %v", err)
	}
	if got.Br["k"] != 1 || got.Ab["z"] != 2 {
		t.Errorf("got %+v", got)
	}
}

// A JSON object used to fill a *multipart.FileHeader, passing for an upload.
// A file arrives only in a multipart form.
func TestJSONCannotFakeAnUpload(t *testing.T) {
	var got struct {
		Avatar *multipart.FileHeader   `json:"avatar"`
		Docs   []*multipart.FileHeader `json:"docs"`
	}
	err := bindJSONBody(t, `{"avatar":{},"docs":[{}]}`, &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 2 || got.Avatar != nil || got.Docs != nil {
		t.Errorf("got %v, Avatar=%v Docs=%v", err, got.Avatar, got.Docs)
	}
}

type threeInts struct {
	A int `json:"a"`
	B int `json:"b"`
	C int `json:"c"`
}

// A struct member sent again after a copy that failed does not bind the
// repeat over it: the failure stands and the bind fails, so nothing from the
// rejected copy passes as bound.
func TestRepeatAfterFailedCopyFails(t *testing.T) {
	for _, body := range []string{
		`{"n":{"b":1,"a":"x"},"n":{"c":2}}`,
		`{"n":{"b":1,"a":"x"},"n":null}`,
		`{"in":{"n":{"b":1,"a":"x"},"n":{"c":2}}}`,
	} {
		var got struct {
			N  threeInts `json:"n"`
			In struct {
				N *threeInts `json:"n"`
			} `json:"in"`
		}
		if err := bindJSONBody(t, body, &got); err == nil {
			t.Errorf("%s: got nil, want the failure to stand", body)
		}
	}
}

// A file part named like a map entry used to be dropped without a word. It
// binds into a map of files and is refused by any other map.
func TestFilePartInFormMap(t *testing.T) {
	build := func() *http.Request {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		_ = w.WriteField("meta[a]", "x")
		fw, _ := w.CreateFormFile("meta[b]", "b.txt")
		_, _ = fw.Write([]byte("hello"))
		_ = w.Close()
		r := httptest.NewRequest("POST", "/", &buf)
		r.Header.Set("Content-Type", w.FormDataContentType())
		return r
	}
	var text struct {
		Meta map[string]string `body:"meta"`
	}
	if err := BindWithOptions(build(), &text, BindOptions{DisallowUnknownFields: true}); err == nil {
		t.Error("text map: got nil, want the file part refused")
	}
	var files struct {
		Meta map[string]*multipart.FileHeader `body:"meta"`
	}
	r := build()
	if err := Bind(r, &files); err == nil {
		t.Error("file map: got nil, want the text part refused")
	}
}

// A failure's message quoted the client's whole value, twice over for a
// nested one: 100 long bad strings held 41 MB of error. A message quotes at
// most maxMessageValue bytes of it.
func TestMessagesClipLongValues(t *testing.T) {
	long := `"` + strings.Repeat("\u0080", 50_000) + `"`
	body := `{"n":[` + strings.Repeat(long+",", 99) + long + `]}`
	var got struct {
		N []int `json:"n"`
	}
	err := BindWithOptions(regressJSONRequest(body), &got, BindOptions{MaxBodySize: -1})
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != maxFailures {
		t.Fatalf("got %d failures", len(errs))
	}
	if n := len(err.Error()); n > maxFailures*(maxMessageValue+64) {
		t.Errorf("message is %d bytes", n)
	}
	if !strings.HasSuffix(errs[0].Message, "…") {
		t.Errorf("message %.80q… not clipped", errs[0].Message)
	}
}

type kilo struct{ B [1024]byte }

// Slices started with room for four, so a list of one-element lists of large
// values held four times what it needed.
func TestSlicesStartSmall(t *testing.T) {
	body := `{"s":[` + strings.Repeat(`[{}],`, 19_999) + `[{}]]}`
	var got struct {
		S [][]kilo `json:"s"`
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := BindWithOptions(regressJSONRequest(body), &got, BindOptions{MaxBodySize: -1}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	// 20,000 one-element lists of 1 KB each are 20 MB; allow for the outer
	// slice's growth, not for four times the inner ones.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 40<<20 {
		t.Errorf("allocated %d MB", alloc>>20)
	}
}

// A MaxBodySize of MaxInt64 overflowed the buffer's growth check and panicked
// once a body outgrew its first buffer.
func TestMaxInt64LimitGrowsSafely(t *testing.T) {
	body := `{"s":"` + strings.Repeat("a", 200_000) + `"}`
	r := regressJSONRequest(body)
	r.ContentLength = -1 // chunked: sized by growth, not by the declared length
	var got struct {
		S string `json:"s"`
	}
	if err := BindWithOptions(r, &got, BindOptions{MaxBodySize: math.MaxInt64}); err != nil || len(got.S) != 200_000 {
		t.Errorf("got %v, len %d", err, len(got.S))
	}
}

// A long unknown key is shortened in its message, and text that is not
// UTF-8 is clipped where it is rather than to nothing.
func TestLongUnknownKeyAndInvalidUTF8Clip(t *testing.T) {
	var got struct {
		A int `json:"a"`
	}
	key := strings.Repeat("k", 100_000)
	err := BindWithOptions(regressJSONRequest(`{"`+key+`":1}`), &got, BindOptions{DisallowUnknownFields: true})
	if err == nil || len(err.Error()) > 1000 {
		t.Errorf("got a %d-byte error", len(fmt.Sprint(err)))
	}
	if got := clipText(strings.Repeat("\x80", 100), 10); len(got) < 7 {
		t.Errorf("clipText of invalid UTF-8 gave %q", got)
	}
}

// A multipart name sent as both text and a file is refused for a field that
// binds it, not a silent choice of one, and ignored where nothing binds it.
// Both a text field and a file field are checked: whichever part silently
// won, one of them would bind it without a failure.
func TestMultipartTextAndFileSameName(t *testing.T) {
	build := func() *http.Request {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		_ = w.WriteField("doc", "text")
		fw, _ := w.CreateFormFile("doc", "d.txt")
		_, _ = fw.Write([]byte("file"))
		_ = w.Close()
		r := httptest.NewRequest("POST", "/", &buf)
		r.Header.Set("Content-Type", w.FormDataContentType())
		return r
	}
	var got struct {
		Doc string `body:"doc"`
	}
	var errs BindErrors
	if err := Bind(build(), &got); !errors.As(err, &errs) || errs[0].Name != "doc" {
		t.Errorf("text field: got %v, want a failure for doc", err)
	}
	var gotFile struct {
		Doc *multipart.FileHeader `body:"doc"`
	}
	if err := Bind(build(), &gotFile); !errors.As(err, &errs) || errs[0].Name != "doc" {
		t.Errorf("file field: got %v; want a failure for doc", err)
	}

	// A struct that does not bind the name is not affected by it.
	var buf2 bytes.Buffer
	w2 := multipart.NewWriter(&buf2)
	_ = w2.WriteField("attachment", "text")
	fw2, _ := w2.CreateFormFile("attachment", "a.txt")
	_, _ = fw2.Write([]byte("file"))
	_ = w2.WriteField("name", "n")
	_ = w2.Close()
	r2 := httptest.NewRequest("POST", "/", &buf2)
	r2.Header.Set("Content-Type", w2.FormDataContentType())
	var other struct {
		Name string `body:"name"`
	}
	if err := Bind(r2, &other); err != nil || other.Name != "n" {
		t.Errorf("unbound name: got %v, Name=%q", err, other.Name)
	}
}

// A query or form map key's failure used to be erased by a later spelling
// of the same key that converted: ?m[01]=x&m[1]=5 returned nil. It stands, as
// in a JSON object.
func TestQueryMapSpellingFailureStands(t *testing.T) {
	var got struct {
		M map[int]int `query:"m"`
	}
	if err := Bind(httptest.NewRequest("GET", "/?m[01]=x&m[1]=5", nil), &got); err == nil {
		t.Error("got nil, want the failure for m[01]")
	}
}

// A repeated bad key whose value never equals itself, such as NaN, was
// reported once per occurrence. It is reported once.
func TestNaNKeyFailureReportedOnce(t *testing.T) {
	var got struct {
		M map[float64]int `json:"m"`
	}
	var errs BindErrors
	if err := bindJSONBody(t, `{"m":{"NaN":"x","NaN":"y"}}`, &got); !errors.As(err, &errs) || len(errs) != 1 {
		t.Errorf("got %v, want one failure", err)
	}
}

// A plain name=value for a map field has no key. A form used to fail on it,
// losing the name[key] entries beside it; it is ignored, as in a query.
func TestPlainValueForFormMapIsIgnored(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader("m=x&m[a]=1"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var got struct {
		M map[string]int `body:"m"`
	}
	if err := Bind(r, &got); err != nil || got.M["a"] != 1 {
		t.Errorf("got %v, M=%v; want nil and a:1", err, got.M)
	}
}

// Whether a JSON number is whole was judged after converting it to float64,
// which rounds: 2.0000000000000001 bound as 2, and 9007199254740993.0 as
// ...992. It is judged exactly from the number's text.
func TestWholeNumbersJudgedExactly(t *testing.T) {
	for body, want := range map[string]int64{
		`{"n":1e5}`: 100000, `{"n":2.50e1}`: 25, `{"n":-3.0}`: -3, `{"n":0.0e-99999999999}`: 0,
		`{"n":9007199254740993.0}`: 9007199254740993, `{"n":9.223372036854775807e18}`: math.MaxInt64,
	} {
		var got struct {
			N int64 `json:"n"`
		}
		if err := bindJSONBody(t, body, &got); err != nil || got.N != want {
			t.Errorf("%s: got %v, %d; want %d", body, err, got.N, want)
		}
	}
	for _, body := range []string{
		`{"n":2.0000000000000001}`, `{"n":1.5}`, `{"n":1e-5}`, `{"n":1e19}`,
		`{"n":1e99999999999999999999}`, `{"n":1e-99999999999999999999}`, `{"u":-1.0}`, `{"u":1.5}`,
	} {
		var got struct {
			N int64  `json:"n"`
			U uint64 `json:"u"`
		}
		if err := bindJSONBody(t, body, &got); err == nil {
			t.Errorf("%s: bound N=%d U=%d, want an error", body, got.N, got.U)
		}
	}
}

// A required member that was sent but failed is reported for its failure,
// not as missing as well: it was present.
func TestFailedRequiredMemberIsNotMissing(t *testing.T) {
	var got struct {
		Age int `body:"age,required"`
	}
	var errs BindErrors
	err := bindJSONBody(t, `{"age":"x"}`, &got)
	if !errors.As(err, &errs) || len(errs) != 1 || errors.Is(errs[0], ErrMissingRequired) {
		t.Errorf("got %v, want one conversion failure", err)
	}
}

// Which members were sent is tracked past a struct's 64th field too, so a
// required field there that was sent is not reported missing, at the top
// level or nested.
func TestRequiredPastSixtyFourthField(t *testing.T) {
	fields := make([]reflect.StructField, 70)
	for i := range fields {
		tag := fmt.Sprintf(`body:"f%d"`, i)
		if i == len(fields)-1 {
			tag = fmt.Sprintf(`body:"f%d,required"`, i)
		}
		fields[i] = reflect.StructField{Name: fmt.Sprintf("F%d", i), Type: reflect.TypeFor[int](), Tag: reflect.StructTag(tag)}
	}
	wide := reflect.StructOf(fields)
	nested := reflect.StructOf([]reflect.StructField{{Name: "N", Type: wide, Tag: `body:"n"`}})

	if err := bindJSONBody(t, `{"f69":1}`, reflect.New(wide).Interface()); err != nil {
		t.Errorf("top level: %v", err)
	}
	if err := bindJSONBody(t, `{"n":{"f69":1}}`, reflect.New(nested).Interface()); err != nil {
		t.Errorf("nested: %v", err)
	}
	if err := bindJSONBody(t, `{"n":{"f0":1}}`, reflect.New(nested).Interface()); !errors.Is(err, ErrMissingRequired) {
		t.Errorf("nested, f69 absent: got %v, want ErrMissingRequired", err)
	}
}

// An omitempty member past the failure budget is not decoded, like any other,
// though only the check before each member stops it: its decoding has no
// check of its own.
func TestOmitEmptyPastBudgetIsSkipped(t *testing.T) {
	var got struct {
		A []int       `body:"a"`
		C countedFail `body:"c,omitempty"`
	}
	body := `{"a":[` + strings.Repeat(`"x",`, maxFailures-1) + `"x"],"c":"x"}`
	failCount.Store(0)
	if err := bindJSONBody(t, body, &got); err == nil {
		t.Fatal("got nil")
	}
	if n := failCount.Load(); n != 0 {
		t.Errorf("converted %d values past the budget, want none", n)
	}
}
