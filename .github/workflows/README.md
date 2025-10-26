# GitHub Actions Workflows

This directory contains GitHub Actions workflows for continuous integration and code quality checks.

## Workflows

### CI Workflow (`ci.yaml`)

The CI workflow runs on every push to `main` and on all pull requests. It consists of two parallel jobs:

#### 1. Lint Job (`golangci-lint`)

Runs golangci-lint to check code quality and style.

- **Runs on:** `ubuntu-latest`
- **Go version:** 1.25 (latest stable)
- **Action:** `golangci/golangci-lint-action@v6`
- **golangci-lint version:** v2.5.0
- **Timeout:** 5 minutes

**Features:**
- Uses official golangci-lint GitHub Action
- Version pinned to v2.5.0 (matches local development)
- Automatically caches Go modules and build artifacts
- Automatically caches golangci-lint analysis
- Fast feedback on code quality issues
- Annotates PRs with linting issues inline

**Configuration:**
The linter uses the `.golangci.yaml` configuration file in the repository root.

#### 2. Test Job (`test`)

Runs the test suite across multiple Go versions to ensure compatibility.

- **Runs on:** `ubuntu-latest`
- **Strategy:** Matrix build across Go versions
  - Go 1.23
  - Go 1.24
  - Go 1.25
- **Command:** `make test`

**Features:**
- Tests with race detector enabled (via Makefile)
- Tests with CGO disabled (via Makefile)
- Parallel execution across Go versions
- Fast feedback on compatibility issues

## Local Development

### Running Linting Locally

To run the same linting checks that run in CI:

```bash
# Install/update golangci-lint to the correct version
make linter-update

# Check current version
make linter-version

# Run linting (with automatic version check)
make lint
```

### Running Tests Locally

To run the same tests that run in CI:

```bash
# Run all tests (with race detector and CGO disabled)
make test

# Run tests with race detector only
make test-race

# Run short tests
make test-short

# Generate coverage report
make coverage
```

## Workflow Triggers

Both jobs are triggered by:

1. **Push to main branch:**
   - Validates all commits before they become part of the main history
   - Ensures main branch always passes all checks

2. **Pull requests:**
   - Validates changes before merging
   - Provides feedback directly on the PR
   - Prevents merging code that doesn't meet quality standards

## Caching

The workflows use caching to speed up builds:

### golangci-lint-action Caching

The `golangci/golangci-lint-action@v6` automatically caches:
- Go modules (`~/go/pkg/mod`)
- Build cache (`~/.cache/go-build`)
- golangci-lint analysis cache (`~/.cache/golangci-lint`)

This significantly reduces CI run times for subsequent runs.

### actions/setup-go Caching

The `actions/setup-go@v5` with `check-latest: true` automatically caches:
- Go modules
- Build cache

## Version Pinning

### golangci-lint Version

The golangci-lint version is pinned to `v2.5.0` in the workflow to ensure:
- Consistent linting results across CI and local development
- No surprise failures from linter upgrades
- Controlled upgrade process

To upgrade:
1. Update `GOLANGCI_LINT_VERSION` in `Makefile`
2. Update `version` in `.github/workflows/ci.yaml`
3. Run `make linter-update` locally
4. Test locally with `make lint`
5. Commit and push changes

### Go Version Strategy

- **Lint job:** Uses Go 1.25 (latest stable)
- **Test job:** Matrix tests Go 1.23, 1.24, 1.25
  - Ensures backward compatibility
  - Detects version-specific issues early

## Troubleshooting

### Linting Fails in CI but Passes Locally

**Cause:** Version mismatch between local and CI golangci-lint

**Solution:**
```bash
# Check your local version
make linter-version

# Update to the CI version
make linter-update

# Re-run linting
make lint
```

### Tests Pass Locally but Fail in CI

**Possible causes:**
1. Race conditions (detected by race detector in CI)
2. Platform-specific issues (CI runs on Linux)
3. Missing test cache cleanup

**Solutions:**
```bash
# Run with race detector locally
make test-race

# Clean test cache and re-run
make clean-test-results
make test
```

### CI Takes Too Long

**Optimization tips:**
1. Caching is automatic - no action needed
2. golangci-lint runs in parallel with tests
3. Consider reducing timeout if linting finishes faster
4. Consider running only on latest Go version for faster feedback

## Best Practices

1. **Always run `make lint` before pushing** to catch issues early
2. **Run `make test` locally** to ensure tests pass before CI
3. **Keep golangci-lint version in sync** between Makefile and CI workflow
4. **Review golangci-lint annotations** on PRs - they point to exact issues
5. **Don't ignore linting failures** - they indicate code quality issues

## Configuration Files

- `.golangci.yaml` - golangci-lint configuration (v2 format)
- `Makefile` - Build and test commands
- `linter.go.mod` - Isolated module for linter tool dependencies

## References

- [golangci-lint GitHub Action](https://github.com/golangci/golangci-lint-action)
- [golangci-lint Documentation](https://golangci-lint.run/)
- [GitHub Actions Documentation](https://docs.github.com/en/actions)
