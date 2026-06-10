# CI Scripts

## PR Title Validation and Label Mapping

### Overview

PR titles are validated and labeled by [`.github/workflows/auto-label-pr.yml`](../workflows/auto-label-pr.yml):

1. **Validate** — [`amannn/action-semantic-pull-request`](https://github.com/amannn/action-semantic-pull-request) enforces [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) on the PR title
2. **Label** — `detect-pr-label.js` maps the validated `type` to a release-category label

### Allowed PR title types

```
feat  enh  fix  docs  deps  ci  chore  refactor  test  perf  style  revert  build
```

Examples: `feat: add auth`, `fix(api): handle edge case`, `deps(go): bump module`

Breaking changes use `!` before `:`: `feat!: drop old API`, `fix(scope)!: breaking fix`

### Type to label mapping

| Type | Label |
|------|-------|
| any type with `!` (e.g. `feat!:`, `fix!:`) | `breaking-change` |
| `feat` | `feature` |
| `enh` | `enhancement` |
| `fix` | `bug` |
| `docs` | `documentation` |
| `deps` | `dependencies` |
| `ci`, `chore`, `refactor`, `test`, `style`, `perf`, `revert`, `build` | `release-note-none` |

Labels align with categories in [`.github/release.yml`](../release.yml).

### Files

- `detect-pr-label.js` — maps validated type + title to a single category label
- `detect-pr-label.test.js` — unit tests

### Running tests

Requires Node.js 18+:

```bash
node --test detect-pr-label.test.js
```

### Branch protection

After merging to `main`, add the **Auto-label PRs** workflow as a required status check so invalid titles block merge.
