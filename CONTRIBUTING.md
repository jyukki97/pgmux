# Contributing to pgmux

Thank you for your interest in contributing to pgmux, a PostgreSQL proxy with connection pooling, read/write query routing, and query caching.

## How to Contribute

Contributions are welcome in the form of bug reports, feature requests, documentation improvements, and code changes. Please read through this guide before submitting your contribution.

## Development Setup

### Prerequisites

- Go 1.25 or later
- PostgreSQL (for local development and testing)
- Docker (for integration and E2E tests)

### Building and Testing

```bash
# Build the project
make build

# Run unit tests
make test

# Run linter
make lint
```

## Project Structure

pgmux follows the standard Go project layout with `cmd/` for entry points and `internal/` for private packages. For a detailed breakdown of every package and file, see [CLAUDE.md](CLAUDE.md).

## Code Style

- Follow the standard Go project layout (`cmd/`, `internal/`).
- Use `slog` for all logging. Do not use `log.Printf`.
- Wrap errors with context: `fmt.Errorf("context: %w", err)`.
- Minimize external dependencies -- prefer the standard library when possible.
- Test files live in the same package as the code they test, with the `_test.go` suffix.

## Testing

```bash
# Unit tests
make test

# Integration and Docker E2E tests
make test-integration

# Benchmarks
make bench
```

All new features and bug fixes should include appropriate tests. Integration tests that require Docker are in the `tests/` directory.

## Submitting Changes

1. **Fork** the repository and clone your fork locally.
2. **Create a branch** from `main` using the naming convention:
   ```
   feat/{issue-number}-{short-description}
   ```
   For example: `feat/3-connection-pool-struct`
3. **Commit** using [Conventional Commits](https://www.conventionalcommits.org/):
   ```
   feat(pool): add Acquire/Release (#3)
   fix(router): handle empty query string (#42)
   docs: update configuration examples (#15)
   ```
4. **Open a Pull Request** against `main`:
   - Reference the related issue (e.g., `Closes #3`).
   - Describe what changed and why.
   - Include a test plan explaining how the changes were verified.
5. PRs are **squash merged**. The branch is deleted after merge.

### Guidelines

- Keep each issue and PR focused on a single concern. Do not bundle unrelated changes.
- Critical bug fixes should be submitted as individual hotfix PRs.
- Ensure `make test` and `make lint` pass before submitting.

## Reporting Bugs

Use [GitHub Issues](../../issues) to report bugs. Please include:

- A clear description of the problem.
- Steps to reproduce the issue.
- Expected vs. actual behavior.
- Environment details (Go version, OS, pgmux version).

## License

By contributing to pgmux, you agree that your contributions will be licensed under the [MIT License](LICENSE).
