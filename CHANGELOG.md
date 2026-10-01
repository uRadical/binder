# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.2.0] - 2026-10-01

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
  binds on GET and DELETE as it does on POST, is limited by `MaxBodySize`
  rather than also by `ParseForm`'s 10 MB cap, and a malformed URL query no
  longer fails it as `ErrMalformedBody`.
- **A JSON array or object into a string is an error.** It used to bind Go's
  formatting of the value, such as `[a b]` or `map[k:1]`.
- **JSON is always decoded with `encoding/json/jsontext`.** The fallback
  decoder for toolchains built with `GOEXPERIMENT=nojsonv2` is gone, so binder
  needs the jsonv2 experiment, which Go 1.27 enables by default. A toolchain
  with it turned off will not build binder.
- **`omitempty` has no effect on pointer fields.** It used to skip a JSON
  `false`, `0` or `""` even into a `*bool` or `*int`, so a PATCH setting a
  flag to false was silently dropped. A pointer already tells "not sent" (nil)
  from a zero value, so the option is now ignored on one.
- **A `time.Duration` needs a unit in text.** A query, header, form or JSON
  string value such as `9` used to bind as 9 nanoseconds; it is now an error
  ("missing unit in duration"), except `0`. Send `9ns`, or a JSON number,
  which is still nanoseconds.
- **Fractions do not truncate into integers.** A JSON number such as `1.9`
  bound into an integer field or `time.Duration` used to truncate to `1`; it
  is now an error, as it already was from a query string. Whole numbers in
  float form, `1e3` or `2.0`, still bind.
- **A failed occurrence of a member stands.** A JSON member sent twice binds
  its last occurrence, but if an earlier one failed to convert, that failure
  is reported and later occurrences are only checked for being well-formed, as
  in `encoding/json`. The body used to be decoded into a map first, so only
  the last occurrence was ever looked at.
- **Other frameworks' binding tags are refused.** A field tagged `binding`,
  `uri` or `param`, as Gin and Echo bind by, used to be skipped, leaving it
  unset. Binding now refuses the type with `ErrInvalidTarget`, naming the
  field and what to use instead: `path:` for `uri:` and `param:`, and the
  `required` option and a `Validate` method for `binding:`. `validate:` tags
  are still left alone.
- **Nested options apply.** `required` and `omitempty` inside a nested struct
  used to be ignored; they now behave as they do at the top level.

### Added

- `form:"name"` binds from a form body when it has the key, and otherwise from
  the query string, as `r.FormValue` reads and as Gin's `form` tag does, so one
  field serves a form post and a `GET` query alike. A JSON body is not a form:
  there `form:` reads the query alone.
- A JSON string binds into a `[]byte` as base64, as `encoding/json` encodes
  one; text from a query, header or form is taken as its bytes.
- At most 100 failures are reported per `Bind`. Once that many are recorded,
  a JSON body stops converting values and only checks that the rest of it is
  well-formed, so rejecting a flood of bad values costs about what reading it
  does; a query or form field stops at its own hundredth. A failure path of
  more than 64 parts, a part being a field or key with its index, keeps its
  ends and elides the
  middle.
- `time.Duration` binds from text such as `5s`, `1m30s` or `250ms`, from any
  source including a JSON string, parsed with `time.ParseDuration`. A JSON
  number is still nanoseconds, as `encoding/json` treats it.
- `BindErrors`, the list of field failures Bind returns.

### Changed

- **A pointer field whose value fails to bind stays nil.** It used to be left
  pointing at a zero value, or at the members of a struct that bound before
  one failed, so after a failure a non-nil pointer did not mean a value was
  bound. A pointer set before binding is kept.
- `Validator.Validate` receives `r.Context()`, so rules that depend on the
  caller, a tenant or a deadline can run during binding.
- Unknown fields are reported alongside field failures rather than ending
  binding.
- **Far fewer allocations.** Against the comparison benchmarks, binding a JSON
  body went from 23 allocations to 8, a query string from 14 to 1, and a
  request using every source from 27 to 6, with memory per request down 79
  to 90% and time down 22 to 46%. Path, query, header and cookie values are
  now read without parsing the whole query or Cookie header, with differential
  fuzz tests against `net/url` and `net/http` showing the results match.
  A form body is parsed from the bytes already read rather than through
  `Request.ParseForm`, which halves its allocations: 21 to 11 for the form
  benchmark, and 24 to 11 for a form request using every source.

### Fixed

- A failure's message quoted the client's whole value, so long bad values
  made errors of tens of megabytes. A message quotes at most 256 bytes of it;
  `Err` keeps the cause whole.
- A multipart file part named like a map entry, `meta[b]`, was dropped without
  a word. It binds into a map of `*multipart.FileHeader` and is refused by any
  other map.
- A long map key was repeated in full in the path of every failure beneath
  it, so a 1 MB key over 100 failures held 400 MB. A path now carries at most
  the first 64 bytes of a key.
- A JSON object filled a `*multipart.FileHeader`, passing for an upload. A file
  arrives only in a multipart form.
- A single value into a recursive list type, `type P []P`, wrapped itself as
  a one-element list forever until the stack overflowed, killing the process.
  A single value into such a type is now refused; a slice of slices that is
  not recursive still takes one as a list of one list.
- A failure deep in a JSON body had its path rebuilt, and every failure below
  it copied, at each level on the way out, so depth multiplied its cost: 5,000
  failures 1,000 levels down took twelve seconds and 83 GB. Paths are now
  joined once, and failures are capped at 100.
- A JSON array or object of the wrong shape, such as an array sent for a
  string, was decoded whole before being refused. It is now skipped and
  reported without being decoded.
- A field whose type is a pointer to itself, `type P *P`, hung binding while
  holding the type cache's lock, so every later `Bind` hung too. It is now
  `ErrInvalidTarget`.
- Map failures with a mix of numeric and other keys came out in a different
  order from run to run; the order is now fixed. Two spellings of one key,
  such as `"01"` and `"1"` into a `map[int]int`, are one entry, and a failure
  in either stands, from JSON, query and form alike.
- A multipart name sent as both text and a file had its text dropped without
  a word. A field that binds such a name now reports it; a name nothing binds
  is ignored, like any other member.
- A large body allocated about five times its size while being read, the
  buffer growing by a quarter at a time; it now doubles, about twice.
- With `DisallowUnknownFields`, each unknown JSON member was checked against
  every earlier one, so a body of many distinct unknown members cost time
  quadratic in their number: 80,000 took six seconds. It is now linear.
- A JSON array or object inside a body, bound into a slice, map or nested
  struct, went through `[]any` and `map[string]any` first, so an 8 MB array
  allocated over 400 MB. Arrays, objects and nested structs are now decoded
  token by token straight into their Go types.
- Embedded pointers that referred to each other hung the search for a
  promoted `Validate`, holding the type cache's lock, so every later `Bind`
  hung too.
- A null element of a pointer slice, `[null, 1]` into `[]*int`, was bound as
  a pointer to zero; it stays nil, as in `encoding/json`.
- A promoted `omitempty` field sent empty at the top level of a JSON body
  allocated its embedded pointer; it no longer does, as in a form body.
- A nil embedded `Validator` interface panicked when called, and a request
  with no URL panicked on a query field.
- The body buffer was sized up front from the client's `Content-Length`, so a
  request declaring a large body and sending none made the server allocate up
  to the limit at once, and a limit near `MaxInt64` panicked. At most 64 KB is
  allocated before the body arrives, and the limit arithmetic cannot overflow.
- A `Validate` promoted from an embedded pointer that stayed nil panicked,
  dereferencing it. It is no longer called when the pointer is nil; a type's
  own `Validate` always runs, as does one promoted from an embedded value not
  reached through a nil pointer; a nil embedded `Validator` interface is
  skipped the same way.
- A JSON `null` inside a nested struct allocated an embedded pointer on the
  way to its field. It sets nothing, as elsewhere.
- A type with its own JSON decoding was refused, such as a money type with
  `UnmarshalJSON` ("cannot set struct field with value of type json.Number")
  or `json.RawMessage`. A type implementing `json.Unmarshaler` or json/v2's
  `UnmarshalerFrom` now decodes its value itself, at any depth, handed the
  bytes exactly as sent. A JSON string still goes to
  `UnmarshalText` when the type has both.
- Maps were refused as an unsupported type, so a `map[string]string` of
  metadata could not be bound. A map now binds from a JSON object, and from a
  query string or form body written as `name[key]=value` pairs (OpenAPI's
  `deepObject` style). Keys may be any type that converts from text: strings, numbers, bools or `TextUnmarshaler`s,
  each value converts as a field of the element type would, and a bad entry is
  reported under the name the client sent, such as `min[price]`. A field of
  type `any` takes the decoded value, numbers as `json.Number`.
- Embedded structs were skipped without a word, so a request type embedding
  a shared `Paging` bound nothing into it. An untagged embedded struct, or
  pointer to one, now has its fields promoted as in `encoding/json`: they bind
  from every source, an embedded pointer is allocated only when one of its
  fields is sent (a null is not), an outer field shadows a promoted one, and a failure
  names the field by its path, such as `Paging.Limit`. An embed tagged
  `json:"-"` is left out.
- `body:"x"` and `json:"x"` on two fields were treated as different keys,
  though both read the body member `x`: a JSON body filled only the later
  field and a form body filled both. They are now one key, and the first
  declared field binds it.
- A form or multipart field sent more than once, bound into a field taking one
  value, bound as the text `[x y]` into a string and failed for any other
  type. It now takes the first value, as a query parameter or header does.
- A single value bound only into a `[]string`. A form value or JSON scalar
  now binds as a one-element slice of any element type, except a JSON string
  into a `[]byte`, which is base64. Into a slice of slices it is a list of one
  list; only a recursive type such as `type P []P` refuses one.
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

[Unreleased]: https://github.com/uRadical/binder/compare/v1.2.0...HEAD
[1.2.0]: https://github.com/uRadical/binder/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/uRadical/binder/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/uRadical/binder/releases/tag/v1.0.0
