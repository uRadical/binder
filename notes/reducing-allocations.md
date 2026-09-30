# Reducing allocations in binder

Working notes, 2026-09-30. Written as raw material for a blog post, so they
record how each number was found as well as the number.

## The starting point

The comparison benchmarks in `benchmarks/` show binder is the fastest of the
reflective binders but allocates the most:

| Scenario | Binder | Echo | Gin | Stdlib by hand |
|----------|-------:|-----:|----:|---------------:|
| JSON body, 5 fields | 23 | 8 | 8 | 8 |
| Query string, 5 fields | 14 | 8 | 9 | 7 |
| Path, query, body, header, cookie | 27 | 15 | 21 | - |

(allocs/op, Go 1.27, Apple M-series.)

Being fastest while allocating the most is itself worth a paragraph. The
per-type tag cache and the JSON walk that writes straight into fields save
more time than the extra allocations cost. Allocations still matter, though:
they add up as GC pressure under load, which a microbenchmark doesn't show.

## How the allocations were found

1. **Count:** `go test -bench . -benchmem` gives allocs/op.
2. **Attribute:** run with every allocation recorded, then rank by count
   rather than bytes:

   ```sh
   go test -run '^$' -bench '^BenchmarkJSON_Binder$' -benchmem \
       -benchtime 200000x -memprofile mem.prof -memprofilerate 1 -o bench.test
   go tool pprof -sample_index=alloc_objects -top bench.test mem.prof
   ```

   Divide each count by the iteration count (200,000) to get allocs/op.
   `-memprofilerate 1` records every allocation rather than a sample, and
   `alloc_objects` ranks by count. For allocs/op, the count is what matters.

3. **Mind the tiny allocator.** The profile only accounted for 16 of the 23
   allocs/op. The runtime packs small pointer-free allocations (under
   16 bytes, such as short strings) into shared 16-byte blocks. `-benchmem`
   counts each small allocation, but the profile sees only each block. Short
   JSON member names like `"age"` disappear into this gap. So the profile
   shows `jsontext.Token.String` at 2/op, when it is really 8 strings.
   Account for tiny allocations by reasoning from the code, or by isolating
   them with `testing.AllocsPerRun`.

4. **Check the Go facts in isolation** with `testing.AllocsPerRun` before
   building on them (throwaway test in the scratchpad):

   | Operation | allocs |
   |-----------|-------:|
   | returning a `string` as `any` | 1 |
   | map lookup `m[string(b)]` with `b []byte` | 0 |
   | `r.Header.Get("X-Request-ID")` (non-canonical name) | 1 more than indexing |
   | `r.Cookie("session")` | 2, on every call |

5. **Count the harness.** Every benchmark iteration re-arms the body
   (`strings.NewReader` plus `io.NopCloser`, 2 allocations), and the target
   escapes to the heap once it is passed as `any` (1 allocation). Those 3 are
   in every library's figure, so no binder can go below them in these
   benchmarks.

## Where binder's allocations come from

### JSON body, 23 allocs/op

| Source | allocs | Notes |
|--------|------:|-------|
| Benchmark harness | 3 | same for every library |
| Read body under the size limit | 2 | `io.LimitReader`, `io.ReadAll` buffer |
| Restore body for later readers | 2 | `bytes.NewBuffer` + `io.NopCloser` |
| Parse `Content-Type` | 1 | `strings.Split` builds a slice to read the first element |
| New `jsontext.Decoder` | 5 | decoder, `bytes.NewReader`, read buffer, state stack, name stack |
| `bound []bool` | 1 | which fields the walk filled |
| Member names | 5 | `Token.String()` just to look the name up in a map |
| String values | 3 | `name`, `city`, `sort` |
| **Accounted for** | **22** | the last one is likely another tiny allocation |

### Query string, 14 allocs/op

| Source | allocs | Notes |
|--------|------:|-------|
| Benchmark harness | 1 | target escapes |
| `url.ParseQuery` | 7 | builds a `url.Values` map with a slice per key, then binder reads 5 of them |
| Boxing values into `any` | 5 | `extractFieldValue` returns `any`, and a `string` stored in `any` is heap-allocated |
| Empty body | 1 | `make(map[string]any)` returned for a request with no body |

The boxing is the surprise. It isn't JSON or reflection. It's a function
signature, `func extractFieldValue(...) (any, bool, error)`, that costs one
allocation per path, query, header or cookie field on every request.

### Mixed sources, 27 allocs/op

The JSON costs above, plus: query parsing (3), boxing for path, query,
header and cookie (4), `r.Cookie` re-parsing the Cookie header (2), and
`Header.Get` canonicalising `X-Request-ID` (1).

## The plan

In order of payoff for risk. Numbers are estimates from the attribution
above; measure each step, since that is the story the post will tell.

| # | Change | JSON | Query | Mixed | Risk |
|---|--------|-----:|------:|------:|------|
| 1 | Don't box `string` values into `any` for path, query, header and cookie: pass strings to a string setter directly | | -5 | -4 | Low |
| 2 | Read member names with `ReadValue` and look them up with `m[string(raw)]`, falling back to unquoting only for names with escapes | -5 | | -2 | Low |
| 3 | Reuse decoders from a `sync.Pool` (`Decoder.Reset`), with the `bytes.Reader` pooled alongside | -5 | | -5 | Low; must not keep a request's body alive in the pool |
| 4 | `strings.Cut` for `Content-Type`; nil map for an empty body | -1 | -1 | -1 | None |
| 5 | Read the body with a hand-written bounded loop (no `LimitReader`); restore it with one `ReadCloser` type (no `NopCloser` wrapper) | -2 | | -2 | Low; the limit tests already pin the behaviour |
| 6 | Track filled fields in a fixed-size bitset instead of `make([]bool, n)` | -1 | | -1 | None, with a slice fallback past 64 fields |
| 7 | Scan `r.URL.RawQuery` for the type's query keys instead of building `url.Values`; values without escapes become substrings of `RawQuery` | | -7 | -3 | Medium: must match `url.ParseQuery` exactly (`;` rejected, bad escapes dropped, first value wins) |
| 8 | Canonicalise header names once, when the type is cached, and index `r.Header` directly | | | -1 | None |
| 9 | Scan the Cookie header for the wanted name instead of `r.Cookie` | | | -2 | Medium: must match `net/http`'s cookie parsing and validation |

### Projected result

| Scenario | Now | After | Echo | Gin | Stdlib by hand |
|----------|----:|------:|-----:|----:|---------------:|
| JSON body | 23 | ~9 | 8 | 8 | 8 |
| Query string | 14 | ~1 | 8 | 9 | 7 |
| Mixed | 27 | ~6 | 15 | 21 | - |

### What can't go

These would remain in the JSON case:

- **The 3 harness allocations.** They're in every library's number.
- **The body buffer.** The body must be read before it can be parsed.
- **One allocation to restore the body.** Keeping `r.Body` readable after
  binding is a feature. Echo and Gin don't do it, which is part of why they
  allocate less.
- **One string per string field.** The field has to own its value.

Removing that last one would mean making field strings substrings of one body
string. That trades allocations for memory retention: keep one field and you
keep the whole body alive. That isn't a good trade for a request binder. It's
worth a paragraph in the post, as the point where fewer allocations stops
being better.

## Angles for the post

- **"Fastest but most allocations"** is a real and common trade-off. The
  benchmarks show both, and each needs a different fix.
- **The biggest single cost wasn't where you'd guess.** It wasn't reflection
  or JSON, but `string` to `any` in an internal return type.
- **The profiler hides tiny allocations.** Profile totals don't add up to
  `-benchmem` because of the tiny allocator. Worth explaining, because it
  confuses everyone the first time.
- **Features cost allocations.** Body size limits and body restoration each
  cost allocations the frameworks don't pay. Compare like with like.
- **Stop point.** Where fewer allocations stops being better (substring
  retention), and how `jsontext`'s `bytes.Buffer` fast path and
  `Decoder.Reset` make the low-risk wins cheap.

## Results

allocs/op, with median ns/op of 5 runs in brackets, from the comparison
benchmarks. After each step: vet, tests under `-race`, the `nojsonv2` build
and staticcheck all pass.

| Step | JSON | Query | Mixed |
|------|-----:|------:|------:|
| Baseline | 23 (813) | 14 (678) | 27 (1,139) |
| 1. No boxing of string sources | 23 (807) | 9 (491) | 23 (988) |
| 2. Member names from raw bytes | 18 (798) | 9 (496) | 21 (992) |
| 3. Pooled decoders | 12 (669) | 9 (505) | 16 (913) |
| 4. `strings.Cut`, nil map for no body | 11 (648) | 8 (473) | 15 (894) |
| 5. Hand-written body read, one-allocation restore | 9 (569) | 8 (484) | 13 (823) |
| 6. Filled-field set on the stack | 8 (550) | 8 (488) | 12 (841) |
| 7. Scan the raw query | 8 (-) | 1 (522) | 9 (734) |
| 8. Canonical header names resolved once | 8 | 1 | 8 |
| 9. Scan the Cookie header | 8 (575) | 1 (539) | 6 (571) |

### Step 1: string sources without `any`

Single values from the path, query, a header or a cookie now go through
`stringValue` and `setFromString`, which parse a string straight into a
predeclared field (`string`, the int, uint and float kinds, `bool`). Anything
else, such as a pointer, a named type or a `TextUnmarshaler`, still takes the
general path, so its behaviour is unchanged.

- The query case went from 14 to 9 allocations, exactly the 5 predicted, and
  got 28% faster. The allocations were only half of that saving: the rest is
  skipping the type switches in `setField`, which a known string never needed.
- Binder's own `BindManyQueryParams` (8 fields) went from 20 to 12.
- New `binder_stringsource_test.go` pins every destination kind, the
  conversion errors and the empty-cookie case. It passes on the old code too,
  which is the point: it shows behaviour didn't change.

### Step 2: member names from raw bytes

`jsonBodyInto` reads each member name with `ReadValue`, which returns the
quoted bytes without allocating, and looks it up with
`info.bodyFields[string(name)]`, which the compiler does without allocating.
Only a name written with escapes, such as `"\u0061ge"`, is decoded with
`jsontext.AppendUnquote`. An unknown name is copied to a string only when
`DisallowUnknownFields` needs to report it.

- JSON went from 23 to 18 (its 5 names) and mixed from 23 to 21 (its 2).
- Time was flat within noise.
- **Noise trap:** the first measurement after this change showed every
  benchmark 60% slower, including the query benchmark, which this change
  cannot affect. Re-running twice gave the usual figures. From here, each
  step is measured twice. For the post: when a change you didn't touch moves,
  suspect the machine before the code.
- New `TestEscapedMemberNames` covers the escaped-name branch, which had no
  test. It runs on both JSON builds.

### Step 3: pooled decoders

Decoders are kept in a `sync.Pool` and reset per request with
`Decoder.Reset`. Each one reads from a `bytes.Buffer` held alongside it in the
pool, which matters twice:

- jsontext documents that it parses a `bytes.Buffer` in place rather than
  copying it into a read buffer of its own. That saves the read-buffer
  allocation, and it means a pooled decoder holds no copy of an old body.
- Before a decoder goes back in the pool its buffer is cleared, so the pool
  never keeps a request's body alive.

The `AllowDuplicateNames(true)` option is now built once in a package variable.
Building an `Options` value per call allocated too, which is why this step
saved one more than predicted.

- JSON went from 18 to 12 and mixed from 21 to 16. JSON also got about 16%
  faster, from about 800 to 670 ns.
- **Checked before trusting it:** because the decoder parses in place, the
  bytes it reads are the same bytes that back the restored `r.Body`. If
  decoding ever rewrote them, for example while unescaping, a later reader
  would see a corrupted body. New `TestJSONDecodingLeavesBodyIntact` binds a
  body full of escapes three times over, on reused decoders, and checks the
  restored body byte for byte. The existing concurrency tests exercise the
  pool under `-race`.

### Step 4: `strings.Cut`, and no map for an absent body

`parseContentType` used `strings.Split` to build a slice of the header's
parameters, only to read the first one without an `=`. It now walks them with
`strings.Cut`. A request with no body, or a body in a format binder doesn't
parse, now yields a nil map instead of an empty one. Nothing writes to it, and
reading a nil map in Go is safe.

- One allocation off every case. Form bodies lost two, because that path
  parses the Content-Type twice.
- To show the `Cut` rewrite matches `Split` exactly, the `parseContentType`
  table gained the awkward cases: a parameter before the media type, only
  parameters, `;;`, a trailing `;` and uppercase. These were checked against
  the old code first, so the expectations are the old behaviour, not a guess.

### Step 5: reading and restoring the body

`readBody` was `io.ReadAll(io.LimitReader(r.Body, limit+1))`. It's now a loop
over `r.Body.Read` into one buffer, reading at most one byte past the limit,
which is enough to tell an oversized body from one exactly at the limit. When
`Content-Length` is declared and within the limit, the buffer is allocated at
that size plus one, leaving room to see `EOF` without growing. Without a
limit, the declared length isn't trusted as a size, since nothing bounds it.
A client that understates its length only makes the buffer grow; it can't
get past the limit.

The restored `r.Body` was `io.NopCloser(bytes.NewBuffer(b))`, two
allocations. It's now `replayBody`, a `bytes.Reader` with a `Close` method
embedded in one small struct: one allocation.

- JSON went from 11 to 9 and mixed from 15 to 13, the 2 predicted. JSON is
  now level with Echo and Gin.
- **Bytes fell further than allocations.** `io.ReadAll` starts at 512 bytes
  and doubles, so a 70-byte JSON body cost a 512-byte buffer. JSON dropped
  from 1,240 to 264 B/op (-79%), and time fell about 12% with it. For the
  post: allocs/op and B/op tell different stories, and this change moved B/op
  far more.
- Binder's own multipart benchmark, with a 4 KB upload, went from 90 to 79
  allocations and from 38 KB to 32 KB, since the buffer no longer doubles its
  way up to the upload size.
- New `TestReadBodyEdges` covers:
  - a `Content-Length` longer than the body,
  - a reader that yields one byte at a time (with no length, over the limit,
    and with no limit),
  - data arriving together with `EOF`,
  - a read error part way through, which must not be reported as
    `ErrBodyTooLarge` or `ErrMalformedBody`.

  New `TestBodyRestoredAfterBind` checks the restored body reads back whole
  and closes. Both pass on the old `ReadAll` code too.

### Step 6: the filled-field set on the stack

The JSON walk marks which fields it filled, so the rest can be bound from
other sources. That was `make([]bool, len(fields))` inside the walk,
returned to the caller. Now `BindWithOptions` declares a `[64]bool`, slices
it to the struct's width, and passes it down for the walk to fill. Nothing
along the way keeps the slice, so escape analysis leaves the array on the
stack. A struct wider than 64 fields falls back to `make`.

Planned as a bitset. Passing a slice of a stack array down changed fewer
lines for the same result, and it's easier to read.

- One allocation off JSON and mixed. JSON is now at 8, level with Echo, Gin
  and the hand-written stdlib version.
- **Verify, don't assume:** `go build -gcflags=-m` prints
  `moved to heap: boundArr` if the array escapes. It printed nothing for
  `boundArr`, and the benchmark confirmed the saving. The same output lists
  4 variables that do escape, all `errors.As` targets inside error branches,
  so they cost nothing on a request that binds cleanly. Worth showing in the
  post as how to read `-m` output.
- New `TestBindWideStruct` binds structs of 64, 65 and 130 fields, built with
  `reflect.StructOf`, on both sides of the stack/heap boundary. The last field
  comes from the query, to check that fields the walk didn't fill still bind.

### Step 7: scanning the raw query

`r.URL.Query()` parses the whole query into `url.Values`: a map with a slice
per key, 7 allocations to read 5 values. For a query of up to 64 parameters,
`queryCache` now scans `RawQuery` for each parameter a field asks for.
`nextQueryValue` takes the same steps as `url.ParseQuery`, in the same order.
It skips an empty pair, a pair containing `;`, and a pair whose key or value
is badly escaped (so a later pair can become the first value). It unescapes
with the same `url.QueryUnescape`, which returns its input unchanged, without
allocating, when there's nothing to unescape. A value without escapes is then
a substring of `RawQuery`, so it costs no allocation.

A query of more than 64 parameters is parsed once into `url.Values`, as
before. That bounds the cost of scanning once per field. It also leaves
`net/url`'s parameter limit, added for a CVE (10,000 by default, set with
`GODEBUG=urlmaxqueryparams`), applied by the standard library itself. The only
divergence is a `GODEBUG` limit set below 64, where a scanned query is read
anyway.

**The results, A/B measured.** Old and new code alternated, 10 runs each,
medians:

| | Query, 5 params | Mixed, 1 param |
|---|---|---|
| `url.Values` | 501 ns, 8 allocs | 896 ns, 12 allocs |
| Scan | 522 ns, 1 alloc | 734 ns, 9 allocs |

Binder now allocates nothing to bind a query; the 1 left is the benchmark
harness. But it's about 4% *slower* on the 5-parameter query, because a
per-field scan repeats work one parse did once. Kept: 7 fewer allocations,
and less GC under load, cost 20 ns here, and the mixed case is 18% faster.
This is the one step that traded time for allocations, and the post should
say so.

**An optimisation that made it slower.** Most pairs are for another
parameter, so checking the key first and skipping the `;` check and value
unescaping for non-matches looked like a clear win. A/B'd, it went from
525 to 647 ns. The key check used `strings.ContainsAny(key, "%+")` to spot a
key that needs no unescaping, and that costs more per call than
`url.QueryUnescape`'s own tight loop. Reverted, with a comment saying why.
The first measurement of it was also polluted by a fuzz run that had just
finished, and only the interleaved A/B was trustworthy.

**Proving the scan matches.** This is where a subtle difference would hurt,
so:

- `TestQueryScanMatchesParseQuery` compares `get` and `all` against
  `url.ParseQuery` for every key over a table of awkward queries: empty
  values, no `=`, `=` inside a value, empty pairs, semicolons, bad escapes in
  keys and values, escaped keys, `+`, UTF-8, a truncated `%`, and interleaved
  repeats.
- `FuzzQueryScan` runs the same comparison on random input. Over 3.4 million
  executions found no difference. It's now in the Makefile and CI fuzz runs.
- `TestQueryNotParsedWhenUnused` asserted the old internals (the query parsed
  and cached), so it was replaced with `TestQueryScannedOrParsedOnce`: a
  short query is never parsed, and a long one is parsed once and reused.

### Step 8: canonical header names, once

`r.Header.Get("X-Request-ID")` canonicalises the name to `X-Request-Id` on
every call, and allocates doing so, because the tag isn't already canonical.
The canonical key depends only on the tag, so `typeInfoFor` now works it out
once, into `fieldInfo.HeaderKey`, and lookups index `r.Header` directly.
That's exactly what `Get` and `Values` do after canonicalising.

- Mixed went from 9 to 8.
- The 13 existing header tests already cover what could break: tags in any
  case, lowercase tags, repeated headers, slices and required headers. They
  passed unchanged.

### Step 9: scanning the Cookie header

`r.Cookie(name)` parses every cookie in the header into a `*Cookie` on every
call, allocating a slice and a struct to return one value. `cookieValue` now
scans for the one wanted name and returns its value as a substring of the
header. It mirrors `net/http`'s `readCookies` step for step: trim each line
and part, split on `=`, require the name to be an RFC 7230 token, strip one
pair of surrounding quotes, reject control characters, `"`, `;` and `\`, and
take the first valid match across all Cookie lines. As with the query, a
request with more than 64 cookies is handed to `r.Cookie` itself, so
`net/http`'s cookie limit (3,000 by default, set with
`GODEBUG=httpcookiemaxnum`) applies unchanged.

- Mixed went from 8 to 6, and time dropped noticeably. `r.Cookie` did real
  work to build a whole `Cookie` just for binder to read `.Value`.
- `TestCookieScanMatchesRequestCookie` compares against `r.Cookie` for every
  name over a table: first-wins, multiple header lines, whitespace, quoted,
  empty-quoted and unbalanced quotes, `"` and `\` inside values, control
  characters, no `=`, empty parts, non-token names, every token symbol, and
  non-ASCII.
- `FuzzCookieScan` fuzzes the header, split into two lines, and the name.
  2.66 million executions found no difference. It's in the Makefile and CI.
- `TestCookieScanDefersManyCookies` covers the hand-off past 64 cookies.

## Summary

Final figures, medians of ten runs (`-benchtime 200ms -count 10`):

| Scenario | Before | After | Projected | Echo | Gin |
|----------|-------:|------:|----------:|-----:|----:|
| JSON body | 23 allocs, 1,240 B, 813 ns | 8 allocs, 256 B, 554 ns | ~9 | 8, 681 B, 830 ns | 8, 681 B, 865 ns |
| Query string | 14 allocs, 672 B, 678 ns | 1 alloc, 64 B, 522 ns | ~1 | 8, 544 B, 884 ns | 9, 608 B, 1,124 ns |
| Mixed sources | 27 allocs, 1,928 B, 1,139 ns | 6 allocs, 248 B, 568 ns | ~6 | 15, 1,202 B, 1,333 ns | 21, 1,644 B, 1,488 ns |

Binder went from fastest but most allocations to fastest and fewest (or tied)
in every scenario, and it uses a third or less of the others' memory. The
mixed case, binder's reason to exist, is 2.3x faster than Echo, with 6
allocations against 15.

**Not touched:** form bodies (21 allocs) still go through `net/http`'s
`ParseForm` on a copied request, and multipart (79) through
`multipart.Reader`. Either could be a follow-up post.

Every scenario ended at or below the projection. The projection held because
it came from attributing each allocation to a line of code, not from guessing.
The steps, in the order that makes a good story:

1. The biggest single cost was a return type, `any`, not reflection or JSON.
2. The profiler undercounts small allocations (the tiny allocator). Reason
   from the code where the numbers don't add up.
3. The standard library already had the tools: `Decoder.Reset`, parsing a
   `bytes.Buffer` in place, `strings.Cut`, allocation-free `m[string(b)]`.
4. `io.ReadAll` cost bytes more than allocations: -79% B/op on JSON.
5. Escape analysis is checkable with `-gcflags=-m`, so check it.
6. Re-implementing standard library parsing (query, cookies) is where
   correctness risk lives. Differential fuzzing against the standard library
   turns "I think it matches" into several million inputs showing it does.
7. Not every change is faster: the query scan traded 4% time on one benchmark
   for 7 allocations, and an "obvious" micro-optimisation measured slower and
   was reverted.
8. Noise: two runs at least, and an interleaved A/B for small differences.

Behaviour was pinned throughout: 13 new test functions, 2 new fuzz targets
(`FuzzQueryScan`, `FuzzCookieScan`) and 7 new `parseContentType` cases. Where
checked against the old code, they passed there too, which shows they describe
existing behaviour rather than the new code's.
