package binder

import (
	"net/http"
	"strings"
	"testing"
)

// cookieScanAgrees checks that cookieValue finds what r.Cookie finds, for the
// given Cookie header lines and name.
func cookieScanAgrees(t *testing.T, lines []string, name string) {
	t.Helper()
	r := &http.Request{Header: http.Header{"Cookie": lines}}
	got, ok := cookieValue(r, name)
	c, err := r.Cookie(name)
	if want := err == nil; ok != want || (ok && got != c.Value) {
		t.Errorf("Cookie %q, name %q: cookieValue = %q, %v; r.Cookie = %v, %v", lines, name, got, ok, c, err)
	}
}

// The scan must accept and reject exactly what net/http does.
func TestCookieScanMatchesRequestCookie(t *testing.T) {
	headers := [][]string{
		nil,
		{""},
		{"a=1"},
		{"a=1; b=2; a=3"},          // the first wins
		{"b=2", "a=1"},             // across header lines
		{"  a = 1 ;b=2  "},         // whitespace around names and parts
		{"a= 1"},                   // a space inside the value is kept
		{`a="1"`},                  // quotes are stripped
		{`a="`, `a=""`},            // a lone quote is invalid; empty quotes are empty
		{`a="1`, "a=2"},            // an unbalanced quote is invalid, so the next wins
		{`a=x"y`, `a=x\y`, "a=3"},  // quote and backslash inside are invalid
		{"a=\x7f", "a=\t", "a=ok"}, // control characters are invalid
		{"a", "a="},                // no equals sign; an empty value
		{";;a=1;;"},
		{"a b=1", "a@=1", "a\"=1"}, // names that are not tokens
		{"!#$%&'*+-.^_`|~=1"},      // every token symbol
		{"é=1", "a=é"},             // non-ASCII
	}
	names := []string{"a", "b", "", "a b", "!#$%&'*+-.^_`|~", "é"}
	for _, lines := range headers {
		for _, name := range names {
			cookieScanAgrees(t, lines, name)
		}
	}
}

// Past maxScanCookies the request is handed to r.Cookie, so net/http's limit
// on the number of cookies applies unchanged.
func TestCookieScanDefersManyCookies(t *testing.T) {
	parts := make([]string, maxScanCookies+1)
	for i := range parts {
		parts[i] = "c=1"
	}
	parts[len(parts)-1] = "last=yes"
	cookieScanAgrees(t, []string{strings.Join(parts, "; ")}, "last")
}

func FuzzCookieScan(f *testing.F) {
	for _, seed := range [][2]string{{"a=1; b=2", "b"}, {`a="x"`, "a"}, {"a b=1; a=2", "a"}, {"a=x\\y;a=1", "a"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, header, name string) {
		// A second line exercises multiple Cookie headers.
		first, second, _ := strings.Cut(header, "\n")
		cookieScanAgrees(t, []string{first, second}, name)
	})
}
