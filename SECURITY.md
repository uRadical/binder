# Security Policy

Binder parses untrusted data. Every HTTP request it binds is attacker
controlled: the body, the query string, the headers, the cookies and the path
values all arrive from the network, and binding drives reflection with them.
Reports are welcome.

## Supported Versions

| Version | Supported |
|---------|-----------|
| 1.1.x   | Yes       |
| 1.0.x   | No        |

Fixes land on the latest minor release. There are no backports to earlier
minors, so upgrading is the remedy.

## Reporting a Vulnerability

Report privately through GitHub, using
[Report a vulnerability](https://github.com/uRadical/binder/security/advisories/new)
on the Security tab. That opens a private advisory visible only to the
maintainers.

Please do not open a public issue for a suspected vulnerability.

Include what you have: the request that triggers it, the target struct, the Go
version, and whether it panics, hangs, exhausts memory, or binds data it
should not.

You should get an acknowledgement within a week. If a report is accepted, the
advisory will be published together with the release that fixes it, crediting
you unless you would rather it did not.

## What Counts

These are in scope:

- A panic reachable from `Bind` with any request and any target struct. Binding
  should always return an error instead.
- Unbounded resource use: memory, CPU or goroutines growing with attacker
  controlled input beyond `MaxBodySize`.
- Binding a value into a field that should not have received it, or reporting
  success while a field is silently wrong.
- A data race in the shared field cache.

These are not:

- A panic caused by a target that breaks `Bind`'s contract, such as a nil
  pointer or a non-struct. Those return `ErrInvalidTarget` by design, and
  passing one is a programming error rather than an attack.
- Resource use within a configured `MaxBodySize`. Set it to suit your service;
  the default is 10 MB.
- Behaviour of a `Validator` implementation, which is your code.
- Anything requiring the attacker to control the target struct definition,
  which is compiled in.

## Hardening Notes

- `MaxBodySize` bounds every read. It defaults to 10 MB and applies per call
  through `BindOptions.MaxBodySize`. Setting it to zero removes the limit,
  which is not recommended for an endpoint reachable from the internet.
- `Bind` restores the request body after reading it, so middleware downstream
  still sees the bytes. The buffered copy is held for the life of the request.
- multipart/form-data is not parsed. File uploads need handling before binding.
