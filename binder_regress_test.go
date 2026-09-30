package binder

import (
	"context"
	"errors"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
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
