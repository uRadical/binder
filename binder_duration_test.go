package binder

import (
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A time.Duration used to fail on "5s" as an integer that would not parse.
// Text is parsed with time.ParseDuration, from every source; a JSON number is
// nanoseconds, as encoding/json treats it.
func TestDurationFromText(t *testing.T) {
	var got struct {
		Timeout  time.Duration            `query:"timeout"`
		Retry    *time.Duration           `header:"X-Retry-After"`
		TTL      time.Duration            `body:"ttl"`
		Nanos    time.Duration            `body:"nanos"`
		Steps    []time.Duration          `body:"steps"`
		PerRoute map[string]time.Duration `query:"per"`
	}
	r := httptest.NewRequest("POST", "/?timeout=1m30s&per[login]=250ms", strings.NewReader(`{"ttl":"5s","nanos":1500,"steps":["1s","2h"]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Retry-After", "10s")
	if err := Bind(r, &got); err != nil {
		t.Fatal(err)
	}
	if got.Timeout != 90*time.Second || got.Retry == nil || *got.Retry != 10*time.Second || got.TTL != 5*time.Second || got.Nanos != 1500 {
		t.Errorf("got Timeout=%v Retry=%v TTL=%v Nanos=%v", got.Timeout, got.Retry, got.TTL, got.Nanos)
	}
	if !reflect.DeepEqual(got.Steps, []time.Duration{time.Second, 2 * time.Hour}) || got.PerRoute["login"] != 250*time.Millisecond {
		t.Errorf("got Steps=%v PerRoute=%v", got.Steps, got.PerRoute)
	}
}

func TestDurationFromForm(t *testing.T) {
	var got struct {
		TTL time.Duration `body:"ttl"`
	}
	if err := Bind(formRequest(t, "application/x-www-form-urlencoded", "ttl", "45m"), &got); err != nil {
		t.Fatal(err)
	}
	if got.TTL != 45*time.Minute {
		t.Errorf("TTL = %v, want 45m", got.TTL)
	}
}

// A value ParseDuration rejects, including a bare number from a text source,
// is reported against the field.
func TestBadDurationIsFieldError(t *testing.T) {
	var got struct {
		A time.Duration `query:"a"`
		B time.Duration `query:"b"`
	}
	err := Bind(httptest.NewRequest("GET", "/?a=soon&b=300", nil), &got)
	var errs BindErrors
	if !errors.As(err, &errs) || len(errs) != 2 || errs[0].Name != "a" || errs[1].Name != "b" {
		t.Errorf("got %v, want failures for a and b", err)
	}
}
