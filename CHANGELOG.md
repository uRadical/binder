# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Upgrading

- **`Validator` now takes a context.** Change `Validate() error` to
  `Validate(ctx context.Context) error`; rules that do not need the context
  can ignore it. This breaks existing implementations: a type still declaring
  `Validate() error` no longer satisfies `Validator` and is not validated.
- **Field failures are returned as `BindErrors`.** Bind used to return the
  first field failure as a `*BindError`; it now attempts every field and
  returns all failures as `BindErrors`, even when there is only one. Code
  asserting `err.(*binder.BindError)` no longer matches; use
  `errors.As(err, &bindErrs)` to get the list, or `errors.As` with a
  `*BindError` for the first failure.
- **Unknown fields are `BindErrors` entries.** `DisallowUnknownFields` used to
  return one error listing every unknown key; each key is now its own entry,
  wrapping `ErrUnknownField`. `errors.Is(err, ErrUnknownField)` still works.
- **Nested failures are named by path.** A failure inside a nested struct or
  slice was reported as a message on the parent field; it is now its own
  entry with `Field` and `Name` such as `Inner.A` and `inner.a`, or `IDs[1]`
  and `id[1]`.

- **`BindStruct` is gone.** It exposed an internal detail of nested binding
  through `reflect.Value`; `Bind` binds nested structs itself.
- **The package-level `MaxBodySize` variable is gone.** Pass
  `BindOptions{MaxBodySize: n}` to `BindWithOptions` instead. A zero
  `MaxBodySize` now means `DefaultMaxBodySize` rather than the package
  setting, and a negative value still removes the limit.
- **A target with no body field no longer reads the body.** Such a request
  used to have its body read, size-checked and parsed anyway, so a malformed
  or oversized body failed a bind that never used it. The body is now left
  for the handler, unless `DisallowUnknownFields` is set.
- **Empty values no longer fill a slice.** `?tags=` used to bind `[""]` and
  satisfy `required`; a query parameter or header whose every value is empty
  now counts as absent for a slice, as an empty value already did for a
  field taking one.
- **Trailing JSON is malformed.** A JSON body followed by anything but
  whitespace, such as a second object, is now `ErrMalformedBody`; it used to
  bind from the first value.
- **`json:"-"` is not bound.** It used to bind a body key literally named `-`.
  A tag with an empty name, such as `json:",omitempty"`, now binds under the
  Go field name rather than the key `""`.
- **Form bodies are parsed on every method.** A form-encoded body is parsed
  from the bytes binder read rather than through `Request.ParseForm`, so it
  binds on GET and DELETE as it does on POST, is limited by `MaxBodySize` alone
  rather than also by `ParseForm`'s 10 MB cap, and a malformed URL query no
  longer fails it as `ErrMalformedBody`.
- **A JSON array or object into a string is an error.** It used to bind Go's
  formatting of the value, such as `[a b]` or `map[k:1]`.
- **JSON is always decoded with `encoding/json/jsontext`.** The fallback
  decoder for toolchains built with `GOEXPERIMENT=nojsonv2` is gone, so binder
  needs the jsonv2 experiment, which Go 1.27 enables by default. A toolchain
  with it turned off will not build binder.
- **Nested options apply.** `required` and `omitempty` inside a nested struct
  used to be ignored; they now behave as they do at the top level.

### Added

- `BindErrors`, the list of field failures Bind returns.

### Changed

- `Validator.Validate` receives `r.Context()`, so rules that depend on the
  caller, a tenant or a deadline can run during binding.
- Unknown fields are reported alongside field failures rather than ending
  binding.
- **Far fewer allocations.** Against the comparison benchmarks, binding a JSON
  body went from 23 allocations to 8, a query string from 14 to 1, and a
  request using every source from 27 to 6, with memory per request down 79
  to 90% and time down 23 to 50%. Path, query, header and cookie values are
  now read without parsing the whole query or Cookie header, with differential
  fuzz tests against `net/url` and `net/http` showing the results match.
  A form body is parsed from the bytes already read rather than through
  `Request.ParseForm`, which halves its allocations: 21 to 11 for the form
  benchmark, and 24 to 11 for a form request using every source.

### Fixed

- Embedded structs were skipped without a word, so a request type embedding
  a shared `Paging` bound nothing into it. An untagged embedded struct, or
  pointer to one, now has its fields promoted as in `encoding/json`: they bind
  from every source, an embedded pointer is allocated only when one of its
  fields is sent, an outer field shadows a promoted one, and a failure names
  the field by its path, such as `Paging.Limit`.
- A form or multipart field sent more than once, bound into a field taking one
  value, bound as the text `[x y]` into a string and failed for any other
  type. It now takes the first value, as a query parameter or header does.
- A single value bound only into a `[]string`. A form value or JSON scalar
  now binds as a one-element slice of any element type.
- A JSON number in float form beyond the range of `int64` or `uint64`, such
  as `1e30`, bound as the type's maximum. It is now reported as an overflow.
- `net.IP`, and any other slice type implementing `encoding.TextUnmarshaler`,
  failed to bind from a query parameter or header. It now takes
  one value like any other `TextUnmarshaler`.
- A body over the size limit was left part read, so a later reader saw only
  its unread remainder. The whole body is now restored.
- A field whose type is an interface, such as `encoding.TextUnmarshaler`,
  could panic. It is now reported as an unsupported type.
- With `DisallowUnknownFields`, an unknown key sent twice was reported twice.

- The documentation no longer says binder does not validate. It runs the
  validation a type defines; what it does not provide is a rule language.

## [1.1.0] - 2026-08-23

No exported function changed shape, so this release is source compatible. It
does change behaviour in cases that previously failed quietly, which is the
point of most of it. Read **Upgrading** before taking it.

### Upgrading

1.1.0 is the first release intended for general use; 1.0 ran only on our own
client projects. If you are arriving here fresh, none of this applies to you —
it is written for those two codebases.

- **Request bodies are capped at 10 MB.** Anything larger is rejected with
  `ErrBodyTooLarge` instead of being read into memory. Set `binder.MaxBodySize`
  during initialisation to raise or lower it, or to zero to remove the limit.
  This is the change most likely to be noticed.
- **A body that cannot be parsed is now an error.** Malformed JSON previously
  bound nothing and reported success; it now returns `ErrMalformedBody`.
  Expect new 400s where there were quiet successes with empty fields.
- **Tag options now bind.** A field written `body:"email,omitempty"` searched
  for a key literally named `email,omitempty` and so never bound. Such fields
  will start receiving values, which may surface data a handler previously
  never saw.
- **Chunked request bodies are read.** They were skipped entirely, because the
  body was gated on a positive `Content-Length` and a chunked request declares
  none.
- **Repeated values fill slices.** A query parameter, header or form field
  given more than once now binds every value to a slice field rather than the
  first. Non-slice fields are unaffected.
- **Go 1.27 is required.** The previous release declared `go 1.25.1`; the
  documentation's claim of 1.22+ was never accurate.
- **`errors.As(err, new(*json.SyntaxError))` no longer matches** when built with
  Go 1.27, where `encoding/json` is implemented on json/v2 and returns
  different error types. Test for `ErrMalformedBody` instead.

### Changed

- **Requires Go 1.27.** The module previously declared `go 1.25.1` while the
  documentation claimed 1.22+ and CI listed a 1.22 to 1.25 matrix that, because
  of toolchain resolution, silently ran 1.25.1 for every entry. The
  requirement, the documentation and the matrix now agree.
- **`github.com/google/uuid` is no longer required.** The tests use the
  standard library `uuid` package introduced in Go 1.27, so the module has no
  dependencies at all and `go.sum` is gone. `uuid.UUID` binds through
  `encoding.TextUnmarshaler` exactly as before, so nothing changes for callers
  using either package in their own request types.

### Added

- `header:"name"` binds from request headers, matched case-insensitively.
  Header is last in tag precedence.
- `BindWithOptions` and `BindOptions`, giving a per-call `MaxBodySize` and
  `DisallowUnknownFields`. `Bind` is the zero-options call.
- `BindError`, carrying the `Field`, `Source`, `Name` and underlying `Err` of a
  failure concerning one field, reachable with `errors.As`.
- Sentinel errors for request-level failures: `ErrMalformedBody`,
  `ErrBodyTooLarge`, `ErrInvalidTarget`, `ErrMissingRequired` and
  `ErrUnknownField`. `ErrInvalidTarget` reports a programming error rather than
  a bad request and deserves a 500, not a 400.
- The `,required` tag option, which was documented but had never been
  implemented.
- `MaxBodySize` and `DefaultMaxBodySize`.
- Multi-value binding from query parameters, headers and form fields into
  slice fields.
- `multipart/form-data` bodies. Text parts bind as ordinary body fields and
  file parts bind to `*multipart.FileHeader` or `[]*multipart.FileHeader`. An
  upload counts against `MaxBodySize` and is held in memory rather than spilled
  to a temporary file, so raise the limit deliberately on an upload endpoint.
- A fallback JSON decoder for toolchains built with `GOEXPERIMENT=nojsonv2`,
  where `encoding/json/jsontext` is unavailable. Both produce the same values;
  only their error message text differs, which is not part of the contract.

### Fixed

Most of these were latent: paths that were wrong but that no reported use
reached, needing an input or a struct shape nothing was sending. The two a
caller could meet in ordinary use were tag options not binding outside
`query:`, and the Readme describing an API that did not exist. Nothing here
was reported in the field.

- A tagged unexported field inside a nested struct panicked on assignment.
  Top-level fields were already skipped; nested ones were not, because the two
  nested binders each had their own copy of the loop and neither checked.
- `BindStruct` panicked when given something other than a struct. It now
  reports `ErrInvalidTarget`.
- Tag options broke the lookup key on every source except query, so
  `path:"id,omitempty"`, `body:"email,omitempty"`, `json:`, `cookie:` and the
  same tags on nested struct fields never bound.
- `Bind` panicked instead of returning an error for a nil target, a
  non-pointer, a nil pointer, a pointer to a non-struct, or a nil request. All
  now return `ErrInvalidTarget`.
- A tagged unexported field panicked on assignment. Fields reflection cannot
  set are ignored, as `encoding/json` ignores them.
- A pointer field whose pointer implements `encoding.TextUnmarshaler` panicked
  when reached through a doubly nested struct, because `UnmarshalText` was
  called on a nil pointer.
- Body parse errors were discarded, so a malformed body bound nothing and
  reported success.
- Chunked bodies were dropped, since the read was gated on `Content-Length`.
- Integers beyond 2^53 lost precision: `9007199254740993` bound as
  `9007199254740992`. Slice elements rounded inconsistently, and a number bound
  into a string field arrived in scientific notation. `uint64` values above
  `MaxInt64` could not bind at all.
- `omitempty` was detected by searching all five tags concatenated, so it was
  found spanning a pair such as `body:"omit"` and `json:"empty"`, in a key
  merely named `omitempty`, and on tags the field did not bind from.
- A form field given more than once failed the whole bind with
  `cannot convert []string to slice`, though `parseBody` produced that
  `[]string` deliberately.
- Only the exact media type `application/json` was parsed as JSON. The RFC 6839
  structured syntax suffix is now recognised, so `application/vnd.api+json`,
  `application/hal+json`, `application/problem+json` and `text/json` parse.
  `application/jsonp` and `application/json-rpc` deliberately do not.
- Benchmarks set path parameters in the request context, which `Bind` never
  reads, so path-tagged benchmarks bound nothing and timed the skip.
  `BenchmarkBindPathOnly` reported 56 ns against a real 122 ns.

### Performance

- Binding tags are resolved once per type rather than re-read for every field
  of every request: 18% to 61% faster across the benchmarks, allocations
  unchanged.
- The query string is parsed once per call rather than once per field. Binding
  eight query parameters is 74% faster and allocates 20 times rather than 90.
- A JSON body is read by walking tokens rather than unmarshalling into a map,
  and members no field binds are skipped rather than decoded. Binding a JSON
  body is 24% to 37% faster and allocates less. Requests without a JSON body
  are unaffected.

### Security

- Request bodies are bounded, so a single request can no longer exhaust server
  memory. The limit is enforced while reading rather than from
  `Content-Length`, which the client controls and may understate, and an
  oversized body is rejected rather than truncated.

## [1.0.0]

Initial release. Used internally on client projects rather than published for
general use.

[1.1.0]: https://github.com/uRadical/binder/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/uRadical/binder/releases/tag/v1.0.0
