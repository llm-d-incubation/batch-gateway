# PR Label Detection Scripts

## Files

- `detect-pr-label.js` - Main logic for detecting PR labels from titles
- `detect-pr-label.test.js` - Node.js test suite

## Running Tests

Requires Node.js 18+ (built-in test runner, no dependencies):

```bash
node --test detect-pr-label.test.js
```

## Label Detection Rules

The script detects labels with the following priority:

1. **Breaking changes** (highest priority)
   - `!: description`
   - `type!: description`
   - `type(scope)!: description`
   - `breaking: description`

2. **Conventional commit prefixes**
   - `feat:` → `feature`
   - `fix:` → `bug`
   - `docs:` → `documentation`
   - `deps:` → `dependencies`
   - `enh:` / `improve:` → `enhancement`
   - `test:` / `ci:` / `chore:` → `release-note-none`

3. **Keyword fallback** (for non-conventional titles)
   - Searches for keywords: `fix`, `bug`, `docs`, `feat`, etc.

4. **File-path fallback** (when title has no match; all changed files must match)
   - `docs/` or `*.md` → `documentation`
   - `go.mod`, `go.sum`, `vendor/` → `dependencies`
   - `.github/workflows/`, `test/`, `e2e/`, `*_test.go` → `release-note-none`

## Examples

```javascript
detectFromTitle('feat: add feature')          // → 'feature'
detectFromTitle('fix(api): resolve bug')      // → 'bug'
detectFromTitle('feat!: breaking change')     // → 'breaking-change'
detectFromTitle('Update documentation')       // → 'documentation'
detectFromTitle('Random title')               // → null
detectFromFiles(['docs/guide.md'])            // → 'documentation'
detectFromFiles(['docs/a.md', 'internal/x.go']) // → null
```
