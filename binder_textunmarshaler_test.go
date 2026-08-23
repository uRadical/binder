package binder

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type upperText struct{ V string }

func (u *upperText) UnmarshalText(b []byte) error {
	u.V = strings.ToUpper(string(b))
	return nil
}

func tuReq(t *testing.T, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/u", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// A pointer field reached through a doubly-nested struct used to panic: the
// nested path did not allocate the pointer, and UnmarshalText dereferenced it.
func TestNestedPointerTextUnmarshaler(t *testing.T) {
	type deep struct {
		T *upperText `body:"t"`
	}
	type mid struct {
		D deep `body:"d"`
	}
	var got struct {
		M mid `body:"m"`
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Bind panicked: %v", p)
		}
	}()

	if err := Bind(tuReq(t, `{"m":{"d":{"t":"hello"}}}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.M.D.T == nil {
		t.Fatal("nested pointer was not allocated")
	}
	if got.M.D.T.V != "HELLO" {
		t.Errorf("V = %q, want %q", got.M.D.T.V, "HELLO")
	}
}

func TestTopLevelPointerTextUnmarshaler(t *testing.T) {
	var got struct {
		P *upperText `body:"p"`
	}
	if err := Bind(tuReq(t, `{"p":"hello"}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.P == nil || got.P.V != "HELLO" {
		t.Errorf("P = %+v, want V=HELLO", got.P)
	}
}

func TestSingleNestedPointerTextUnmarshaler(t *testing.T) {
	type inner struct {
		T *upperText `body:"t"`
	}
	var got struct {
		N inner `body:"n"`
	}
	if err := Bind(tuReq(t, `{"n":{"t":"hello"}}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.N.T == nil || got.N.T.V != "HELLO" {
		t.Errorf("N.T = %+v, want V=HELLO", got.N.T)
	}
}

func TestSliceOfPointerTextUnmarshalers(t *testing.T) {
	var got struct {
		P []*upperText `body:"p"`
	}
	if err := Bind(tuReq(t, `{"p":["a","b"]}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.P) != 2 || got.P[0] == nil || got.P[0].V != "A" || got.P[1].V != "B" {
		t.Errorf("P = %v, want [A B]", got.P)
	}
}

// Value-receiver types addressed through the struct still work.
func TestValueTextUnmarshaler(t *testing.T) {
	var got struct {
		When time.Time `body:"when"`
	}
	if err := Bind(tuReq(t, `{"when":"2026-01-02T03:04:05Z"}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if !got.When.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("When = %v, want 2026-01-02T03:04:05Z", got.When)
	}
}

// A value that is not text is still refused rather than guessed at.
func TestNonTextValueIntoTextUnmarshaler(t *testing.T) {
	var got struct {
		When time.Time `body:"when"`
	}
	err := Bind(tuReq(t, `{"when":12345}`), &got)
	if err == nil {
		t.Fatal("got nil error, want a conversion failure")
	}
	if !strings.Contains(err.Error(), "not a string") {
		t.Errorf("error %q does not explain the mismatch", err)
	}
	var bindErr *BindError
	if !errors.As(err, &bindErr) || bindErr.Field != "When" {
		t.Errorf("error does not name the field: %v", err)
	}
}

// A JSON null leaves the field alone rather than unmarshalling from nothing.
func TestNullIntoTextUnmarshaler(t *testing.T) {
	var got struct {
		When time.Time `body:"when"`
	}
	if err := Bind(tuReq(t, `{"when":null}`), &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if !got.When.IsZero() {
		t.Errorf("When = %v, want the zero time", got.When)
	}
}

// A TextUnmarshaler bound from a header or query parameter, not just a body.
func TestTextUnmarshalerFromOtherSources(t *testing.T) {
	var got struct {
		Q *upperText `query:"q"`
		H *upperText `header:"X-Text"`
	}
	r := httptest.NewRequest("GET", "/s?q=alpha", nil)
	r.Header.Set("X-Text", "beta")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Q == nil || got.Q.V != "ALPHA" {
		t.Errorf("Q = %+v, want V=ALPHA", got.Q)
	}
	if got.H == nil || got.H.V != "BETA" {
		t.Errorf("H = %+v, want V=BETA", got.H)
	}
}

// tryTextUnmarshaler must allocate a nil pointer itself. Exercising it through
// a nested struct cannot prove that, because setStruct allocates first; this
// reaches setField directly so only the unmarshaler's own guard is in play.
func TestTextUnmarshalerAllocatesNilPointerDirectly(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("setField panicked on a nil pointer: %v", p)
		}
	}()

	field := reflect.New(reflect.TypeOf((*upperText)(nil))).Elem()
	if !field.IsNil() {
		t.Fatal("fixture is not a nil pointer")
	}

	if err := setField(field, "hello"); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}

	got, ok := field.Interface().(*upperText)
	if !ok || got == nil {
		t.Fatalf("pointer was not allocated: %#v", field.Interface())
	}
	if got.V != "HELLO" {
		t.Errorf("V = %q, want %q", got.V, "HELLO")
	}
}
