# Contributing to Binder

Thank you for your interest in contributing! This library values simplicity and focus - please help us maintain these principles.

## Development Setup

```bash
# Clone the repository
git clone https://github.com/yourusername/binder.git
cd binder

# Run tests
go test ./...

# Run benchmarks
go test -bench=. -benchmem

# Check test coverage
go test -cover
```

## Contribution Guidelines

### What We're Looking For
- **Bug fixes** with test cases demonstrating the issue
- **Performance improvements** with benchmark comparisons
- **Documentation improvements** and clarifications
- **Test coverage** for edge cases

### What We're NOT Looking For
- **New features** that expand scope beyond HTTP binding
- **Validation logic** (keep this in separate libraries)
- **External dependencies** (we want to stay at zero)
- **Breaking API changes** without strong justification

## Pull Request Process

1. **Keep it focused**: One issue per PR
2. **Add tests**: All changes must include appropriate tests
3. **Run benchmarks**: For performance-related changes, include before/after benchmarks
4. **Update docs**: If behavior changes, update godoc comments
5. **Follow style**: Use `gofmt` and follow existing patterns

## Code Style

- Use standard Go conventions
- Keep functions small and focused
- Prefer clarity over cleverness
- Add comments only where the code isn't self-explanatory
- No unused code or commented-out blocks

## Testing Requirements

All PRs must:
- Pass existing tests: `go test ./...`
- Include new tests for new behavior
- Maintain or improve code coverage
- Pass race condition checks: `go test -race ./...`

## Reporting Issues

When reporting issues, please include:
- Go version (`go version`)
- Minimal reproducible example
- Expected vs actual behavior
- Error messages if any

## Philosophy Reminder

Before contributing, remember that Binder:
- Does one thing: binds HTTP data to structs
- Has zero dependencies (and will stay that way)
- Values simplicity over features
- Is designed for Go 1.22+ with native path parameters

If your contribution doesn't align with these principles, consider whether it might be better as a separate library that complements Binder rather than modifying it.

## Questions?

Feel free to open an issue for discussion before starting work on significant changes.