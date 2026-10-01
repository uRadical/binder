package binder

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Gin's binding and uri tags and Echo's param tag would otherwise be skipped
// without a word, leaving the field unset; they are refused instead, naming
// what to use.

func bindForeign(target any) error {
	r := httptest.NewRequest("POST", "/?name=x", strings.NewReader(`{"name":"x"}`))
	r.Header.Set("Content-Type", "application/json")
	return Bind(r, target)
}

func TestForeignTagsAreRefused(t *testing.T) {
	type item struct {
		SKU string `json:"sku" binding:"required"`
	}
	type audit struct {
		By string `param:"by"`
	}
	for name, tc := range map[string]struct {
		target any
		want   string
	}{
		"binding": {&struct {
			Name string `form:"name" binding:"required"`
		}{}, `Name has the tag binding:"required"`},
		"uri": {&struct {
			ID string `uri:"id"`
		}{}, `ID has the tag uri:"id"`},
		"param": {&struct {
			ID string `param:"id"`
		}{}, `ID has the tag param:"id"`},
		"nested element": {&struct {
			Items []item `body:"items"`
		}{}, `Items.SKU has the tag binding:"required"`},
		"embedded": {&struct {
			audit
			Name string `query:"name"`
		}{}, `audit.By has the tag param:"by"`},
		"through a pointer": {&struct {
			Item *item `body:"item"`
		}{}, `Item.SKU`},
	} {
		err := bindForeign(tc.target)
		if !errors.Is(err, ErrInvalidTarget) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want ErrInvalidTarget naming %s", name, err, tc.want)
		}
	}
}

func TestForeignTagMessageSaysWhatToUse(t *testing.T) {
	err := bindForeign(&struct {
		ID string `uri:"id"`
	}{})
	if err == nil || !strings.Contains(err.Error(), `path:"id"`) {
		t.Errorf("got %v, want it to suggest path:\"id\"", err)
	}
	err = bindForeign(&struct {
		Name string `body:"name" binding:"required"`
	}{})
	if err == nil || !strings.Contains(err.Error(), "required option") {
		t.Errorf("got %v, want it to suggest the required option", err)
	}
}

// Tags binder has no reason to refuse are left alone: validate tags, which a
// Validate method may hand to a validation library, and the tags other
// packages read, such as db or yaml. A field that does not bind, and a type
// that decodes itself, are not looked into.
func TestOtherTagsAreAllowed(t *testing.T) {
	type notBound struct {
		X string `binding:"required"`
	}
	type cyclic struct {
		Next *cyclic `body:"next"`
		V    string  `body:"v" validate:"max=3"`
	}
	var got struct {
		Name    string    `body:"name" validate:"required,email" db:"name" yaml:"name"`
		Skip    notBound  // no binder tag, so never bound or inspected
		When    time.Time `query:"when"`
		Cycle   cyclic    `body:"cycle"`
		Private string    `binding:"-"`
	}
	_ = got.Private
	err := bindForeign(&got)
	if err == nil || !strings.Contains(err.Error(), "Private") {
		t.Fatalf("got %v, want only Private refused", err)
	}
	var ok struct {
		Name  string   `body:"name" validate:"required,email" db:"name" yaml:"name"`
		Skip  notBound // no binder tag, so never bound or inspected
		Cycle cyclic   `body:"cycle"`
	}
	if err := bindForeign(&ok); err != nil {
		t.Errorf("got %v", err)
	}
	if ok.Name != "x" {
		t.Errorf("Name = %q", ok.Name)
	}
}
