# Contributing to distribKV

Thank you for your interest in contributing! This document outlines the gitflow workflow and guidelines for contributing to distribKV.

## Gitflow Workflow

We use a simplified Gitflow workflow to manage development and releases.

### Branch Structure

```
main          - Production-ready code, always stable
develop       - Integration branch for features
feature/*     - Feature branches (e.g., feature/log-compaction)
bugfix/*      - Bug fix branches (e.g., bugfix/election-timeout)
hotfix/*      - Hotfix branches for production issues (e.g., hotfix/crash-recovery)
```

### Branching Rules

- `main` is protected and only accepts merges via pull request
- `develop` is the default integration branch
- All work happens on feature branches
- Feature branches branch from and merge back to `develop`
- Hotfixes branch from `main` and merge to both `main` and `develop`

## Workflow

### 1. Feature Development

```bash
# Start from develop
git checkout develop
git pull origin develop

# Create feature branch
git checkout -b feature/your-feature-name

# Make changes and commit
git add .
git commit -m "feat: add log compaction"

# Push to remote
git push -u origin feature/your-feature-name
```

### 2. Bug Fixes

```bash
# Start from develop
git checkout develop
git pull origin develop

# Create bugfix branch
git checkout -b bugfix/issue-description

# Make changes and commit
git add .
git commit -m "fix: resolve election timeout deadlock"

# Push to remote
git push -u origin bugfix/issue-description
```

### 3. Hotfixes (Production Issues)

```bash
# Start from main
git checkout main
git pull origin main

# Create hotfix branch
git checkout -b hotfix/critical-bug

# Fix the issue and commit
git add .
git commit -m "hotfix: prevent data loss during crash recovery"

# Push to remote
git push -u origin hotfix/critical-bug

# Merge to main
git checkout main
git merge --no-ff hotfix/critical-bug
git tag -a v1.0.1 -m "Hotfix: critical bug"

# Merge to develop
git checkout develop
git merge --no-ff hotfix/critical-bug

# Cleanup
git branch -d hotfix/critical-bug
```

## Commit Message Convention

Follow [Conventional Commits](https://www.conventionalcommits.org/) format:

```
<type>(<scope>): <subject>

<body>

<footer>
```

### Types

- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation changes
- `style`: Code style changes (formatting, no logic change)
- `refactor`: Code refactoring
- `perf`: Performance improvements
- `test`: Adding or updating tests
- `chore`: Build process, tooling, dependencies
- `wip`: Work in progress, some work in progress feature that hasn't been finished yet

### Examples

```
feat(raft): implement leader election with randomized timeout

- Add election timeout randomization (150-300ms)
- Implement RequestVote RPC handler
- Add vote granting logic

Closes #12
```

```
fix(persistence): prevent data loss on crash

Ensure all persistent state is written to disk
before responding to RPCs. This prevents loss
of committed entries during crash recovery.

Refs: Raft paper Section 5.4
```

```
test(raft): add election timing tests

Add comprehensive tests for leader election timing,
including cases with network partitions and delays.
```

## Pull Request Process

### Before Opening PR

1. **Update develop**: Ensure your branch is up to date

   ```bash
   git checkout develop
   git pull origin develop
   git checkout feature/your-feature
   git rebase develop
   ```

2. **Run tests**: All tests must pass

   ```bash
   go test -race ./...
   go test -race -cover ./...
   ```

3. **Format code**: Ensure code is properly formatted

   ```bash
   gofmt -w .
   ```

4. **Check imports**: Verify imports are properly organized
   ```bash
   go mod tidy
   ```

### PR Title Format

Use conventional commit format:

- `feat: add log compaction`
- `fix: resolve election deadlock`
- `docs: update Raft implementation notes`

### PR Description Template

```markdown
## Summary

Brief description of changes (2-3 sentences)

## Changes

- Bullet list of main changes
- Highlight any breaking changes
- Note any new dependencies

## Testing

- Tests added/modified
- Manual testing performed
- Test results: `go test -race ./...`

## Related Issues

Closes #123
Refs #456

## Checklist

- [ ] Code follows style guidelines
- [ ] All tests pass with `-race` flag
- [ ] Comments added where needed
- [ ] Documentation updated
- [ ] No new warnings generated
```

## Code Review Guidelines

### For Reviewers

- Check for race conditions (run with `-race`)
- Verify error handling is complete
- Ensure proper lock usage (no deadlocks)
- Check that Raft safety properties are maintained
- Verify tests cover edge cases
- Confirm code style matches AGENTS.md guidelines

### For Authors

- Respond to all review comments
- Update tests for new code paths
- Re-run tests after addressing feedback
- Squash trivial commits before merging
- Keep PRs focused and reasonably sized

## Testing Requirements

### Running Tests

```bash
# All tests with race detector
go test -race ./...

# Specific package
go test -race ./pkg/raft

# Specific test
go test ./pkg/raft -run TestElection -v

# Coverage report
go test -race -cover ./...
```

### Test Coverage

- Aim for >80% code coverage
- All critical paths must be tested
- Include race condition tests for concurrent code
- Test edge cases: network partitions, crashes, timeouts

### Test Organization

- Use table-driven tests for multiple cases
- Test files: `filename_test.go` in same package
- Test names: `TestFunctionName` or `TestScenario_Description`
- Avoid `time.Sleep`; use channels or sync.Cond

## Release Process

### Versioning

We use [Semantic Versioning](https://semver.org/):

- MAJOR: Incompatible API changes
- MINOR: Backwards-compatible functionality
- PATCH: Backwards-compatible bug fixes

### Release Steps

```bash
# Ensure develop is stable
git checkout develop
git pull origin develop

# Run final tests
go test -race ./...
go build -o bin/kvserver ./cmd/raft-kv-server

# Merge to main
git checkout main
git merge --no-ff develop

# Tag release
git tag -a v1.2.3 -m "Release v1.2.3"

# Push
git push origin main
git push origin v1.2.3
```

## Getting Help

- Read [AGENTS.md](./AGENTS.md) for code style guidelines
- Check existing issues and PRs for context
- Reference the [Raft paper](https://raft.github.io/raft.pdf)
- Run tests locally before submitting

## Code of Conduct

- Be respectful and constructive
- Focus on what is best for the community
- Show empathy towards other contributors
- Welcome newcomers and help them learn

## Questions?

Open an issue for questions or discussion. We're here to help!
