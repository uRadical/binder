package binder

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

// multipartBody builds a multipart request body from text fields and files.
func multipartBody(t *testing.T, fields map[string][]string, files map[string][]string) (string, []byte) {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	for name, values := range fields {
		for _, v := range values {
			if err := w.WriteField(name, v); err != nil {
				t.Fatalf("writing field %s: %v", name, err)
			}
		}
	}
	for name, contents := range files {
		for i, content := range contents {
			part, err := w.CreateFormFile(name, "upload"+string(rune('0'+i))+".txt")
			if err != nil {
				t.Fatalf("creating file %s: %v", name, err)
			}
			if _, err := part.Write([]byte(content)); err != nil {
				t.Fatalf("writing file %s: %v", name, err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing writer: %v", err)
	}
	return w.FormDataContentType(), buf.Bytes()
}

func bindMultipart(t *testing.T, target interface{}, fields map[string][]string, files map[string][]string) error {
	t.Helper()
	ct, body := multipartBody(t, fields, files)
	r := httptest.NewRequest("POST", "/upload", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	return Bind(r, target)
}

// Text parts of a multipart form bind like any other body field.
func TestMultipartTextFields(t *testing.T) {
	var got struct {
		Name string   `body:"name"`
		Age  int      `body:"age"`
		Tags []string `body:"tags"`
	}

	err := bindMultipart(t, &got,
		map[string][]string{"name": {"Alice"}, "age": {"30"}, "tags": {"a", "b"}}, nil)
	if err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Name != "Alice" || got.Age != 30 {
		t.Errorf("bound %+v, want Name=Alice Age=30", got)
	}
	if len(got.Tags) != 2 {
		t.Errorf("Tags = %v, want two entries", got.Tags)
	}
}

// A file part binds to a *multipart.FileHeader, and its contents are readable.
func TestMultipartSingleFile(t *testing.T) {
	var got struct {
		Name   string                `body:"name"`
		Avatar *multipart.FileHeader `body:"avatar"`
	}

	err := bindMultipart(t, &got,
		map[string][]string{"name": {"Alice"}},
		map[string][]string{"avatar": {"file contents here"}})
	if err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Name != "Alice" {
		t.Errorf("Name = %q, want %q", got.Name, "Alice")
	}
	if got.Avatar == nil {
		t.Fatal("Avatar is nil, want an uploaded file")
	}
	if got.Avatar.Filename != "upload0.txt" {
		t.Errorf("Filename = %q, want %q", got.Avatar.Filename, "upload0.txt")
	}
	if got.Avatar.Size != int64(len("file contents here")) {
		t.Errorf("Size = %d, want %d", got.Avatar.Size, len("file contents here"))
	}

	f, err := got.Avatar.Open()
	if err != nil {
		t.Fatalf("opening upload: %v", err)
	}
	defer f.Close()
	content, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("reading upload: %v", err)
	}
	if string(content) != "file contents here" {
		t.Errorf("contents = %q, want %q", content, "file contents here")
	}
}

// Several files under one name bind to a slice.
func TestMultipartMultipleFiles(t *testing.T) {
	var got struct {
		Docs []*multipart.FileHeader `body:"docs"`
	}

	err := bindMultipart(t, &got, nil,
		map[string][]string{"docs": {"first", "second", "third"}})
	if err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if len(got.Docs) != 3 {
		t.Fatalf("Docs = %v, want three files", got.Docs)
	}
	if got.Docs[0].Filename != "upload0.txt" || got.Docs[2].Filename != "upload2.txt" {
		t.Errorf("filenames = %q, %q", got.Docs[0].Filename, got.Docs[2].Filename)
	}
}

// One file into a slice field, and several into a single field, both work.
func TestMultipartFileArityMismatch(t *testing.T) {
	var slice struct {
		Docs []*multipart.FileHeader `body:"docs"`
	}
	if err := bindMultipart(t, &slice, nil, map[string][]string{"docs": {"only"}}); err != nil {
		t.Fatalf("one file into a slice: got error %v, want nil", err)
	}
	if len(slice.Docs) != 1 {
		t.Errorf("Docs = %v, want one file", slice.Docs)
	}

	var single struct {
		Doc *multipart.FileHeader `body:"docs"`
	}
	if err := bindMultipart(t, &single, nil, map[string][]string{"docs": {"a", "b"}}); err != nil {
		t.Fatalf("several files into one field: got error %v, want nil", err)
	}
	if single.Doc == nil || single.Doc.Filename != "upload0.txt" {
		t.Errorf("Doc = %+v, want the first file", single.Doc)
	}
}

// A file bound to something that is not a FileHeader is refused, not coerced.
func TestMultipartFileIntoWrongType(t *testing.T) {
	var got struct {
		Avatar string `body:"avatar"`
	}
	err := bindMultipart(t, &got, nil, map[string][]string{"avatar": {"contents"}})
	if err == nil {
		t.Fatal("got nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "uploaded file") {
		t.Errorf("error %q does not explain the mismatch", err)
	}
}

// required and omitempty apply to multipart fields too.
func TestMultipartRequiredFile(t *testing.T) {
	var got struct {
		Avatar *multipart.FileHeader `body:"avatar,required"`
	}
	err := bindMultipart(t, &got, map[string][]string{"name": {"Alice"}}, nil)
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("got %v, want ErrMissingRequired", err)
	}
}

// An upload counts against MaxBodySize, so a service accepting files must
// raise it deliberately.
func TestMultipartRespectsBodyLimit(t *testing.T) {
	withMaxBodySize(t, 512)

	var got struct {
		Doc *multipart.FileHeader `body:"doc"`
	}
	err := bindMultipart(t, &got, nil, map[string][]string{"doc": {strings.Repeat("x", 4096)}})
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("got %v, want ErrBodyTooLarge", err)
	}
}

func TestMultipartWithinBodyLimit(t *testing.T) {
	withMaxBodySize(t, 64<<10)

	var got struct {
		Doc *multipart.FileHeader `body:"doc"`
	}
	if err := bindMultipart(t, &got, nil, map[string][]string{"doc": {strings.Repeat("x", 4096)}}); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Doc == nil || got.Doc.Size != 4096 {
		t.Errorf("Doc = %+v, want a 4096 byte upload", got.Doc)
	}
}

// A malformed multipart body is reported rather than silently ignored.
func TestMalformedMultipartReported(t *testing.T) {
	var got struct {
		Name string `body:"name"`
	}
	r := httptest.NewRequest("POST", "/upload", strings.NewReader("not a multipart body"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=abc123")

	if err := Bind(r, &got); !errors.Is(err, ErrMalformedBody) {
		t.Fatalf("got %v, want ErrMalformedBody", err)
	}
}

// A multipart Content-Type with no boundary cannot be parsed.
func TestMultipartWithoutBoundary(t *testing.T) {
	var got struct {
		Name string `body:"name"`
	}
	r := httptest.NewRequest("POST", "/upload", strings.NewReader("whatever"))
	r.Header.Set("Content-Type", "multipart/form-data")

	if err := Bind(r, &got); !errors.Is(err, ErrMalformedBody) {
		t.Fatalf("got %v, want ErrMalformedBody", err)
	}
}

// Multipart fields sit alongside the other sources.
func TestMultipartAlongsideOtherSources(t *testing.T) {
	var got struct {
		Q      string                `query:"q"`
		Trace  string                `header:"X-Request-ID"`
		Name   string                `body:"name"`
		Avatar *multipart.FileHeader `body:"avatar"`
	}

	ct, body := multipartBody(t,
		map[string][]string{"name": {"Alice"}},
		map[string][]string{"avatar": {"data"}})

	r := httptest.NewRequest("POST", "/upload?q=find", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("X-Request-ID", "trace-1")

	if err := Bind(r, &got); err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if got.Q != "find" || got.Trace != "trace-1" || got.Name != "Alice" || got.Avatar == nil {
		t.Errorf("bound %+v, want every source populated", got)
	}
}
