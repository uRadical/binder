package binder

import (
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The JSON walk records the fields it filled in an array on the stack for up
// to 64 fields, and a slice beyond that. A struct wider than 64 fields binds
// every field, whichever side of the boundary it falls on, and the fields the
// walk did not fill are still bound from their own sources.
func TestBindWideStruct(t *testing.T) {
	for _, width := range []int{64, 65, 130} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			fields := make([]reflect.StructField, width)
			var body []string
			for i := range fields {
				tag := fmt.Sprintf(`body:"f%d"`, i)
				if i == width-1 {
					tag = `query:"last,required"` // not in the body
				}
				fields[i] = reflect.StructField{Name: fmt.Sprintf("F%d", i), Type: reflect.TypeFor[int](), Tag: reflect.StructTag(tag)}
				if i < width-1 {
					body = append(body, fmt.Sprintf(`"f%d":%d`, i, i))
				}
			}
			target := reflect.New(reflect.StructOf(fields))

			r := httptest.NewRequest("POST", "/?last=-1", strings.NewReader("{"+strings.Join(body, ",")+"}"))
			r.Header.Set("Content-Type", "application/json")
			if err := Bind(r, target.Interface()); err != nil {
				t.Fatalf("Bind: %v", err)
			}
			for i := range width - 1 {
				if got := target.Elem().Field(i).Int(); got != int64(i) {
					t.Fatalf("F%d = %d, want %d", i, got, i)
				}
			}
			if got := target.Elem().Field(width - 1).Int(); got != -1 {
				t.Errorf("last field = %d, want -1 from the query", got)
			}
		})
	}
}
