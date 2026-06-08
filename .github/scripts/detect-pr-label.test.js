const { describe, test } = require('node:test');
const assert = require('node:assert/strict');
const { detectFromTitle, detectFromFiles } = require('./detect-pr-label.js');

describe('detectFromTitle', () => {
  describe('conventional commit prefix detection', () => {
    const testCases = [
      ['feat: add feature', 'feature'],
      ['feature: add feature', 'feature'],
      ['fix: bug', 'bug'],
      ['bugfix: fix issue', 'bug'],
      ['bug: resolve issue', 'bug'],
      ['docs: update', 'documentation'],
      ['doc: update', 'documentation'],
      ['deps: update', 'dependencies'],
      ['dependencies: bump', 'dependencies'],
      ['enh: improve', 'enhancement'],
      ['enhancement: improve', 'enhancement'],
      ['improve: performance', 'enhancement'],
      ['test: add tests', 'release-note-none'],
      ['refactor: cleanup', 'release-note-none'],
      ['ci: update workflow', 'release-note-none'],
      ['chore: misc', 'release-note-none'],
      ['style: format', 'release-note-none'],
      ['perf: optimize', 'release-note-none'],
      ['revert: undo change', 'release-note-none'],
    ];

    for (const [title, expected] of testCases) {
      test(`"${title}" → ${expected}`, () => {
        assert.equal(detectFromTitle(title), expected);
      });
    }
  });

  describe('conventional commit with scope', () => {
    const testCases = [
      ['feat(api): add endpoint', 'feature'],
      ['fix(auth): resolve bug', 'bug'],
      ['docs(readme): update', 'documentation'],
      ['test(e2e): add tests', 'release-note-none'],
    ];

    for (const [title, expected] of testCases) {
      test(`"${title}" → ${expected}`, () => {
        assert.equal(detectFromTitle(title), expected);
      });
    }
  });

  describe('breaking change detection', () => {
    const testCases = [
      ['!: breaking change', 'breaking-change'],
      ['feat!: breaking feature', 'breaking-change'],
      ['fix!: breaking fix', 'breaking-change'],
      ['feat(api)!: breaking change', 'breaking-change'],
      ['breaking: major change', 'breaking-change'],
      ['break: api change', 'breaking-change'],
    ];

    for (const [title, expected] of testCases) {
      test(`"${title}" → ${expected}`, () => {
        assert.equal(detectFromTitle(title), expected);
      });
    }
  });

  describe('keyword fallback (no conventional prefix)', () => {
    const testCases = [
      ['Fix the bug', 'bug'],
      ['Update docs', 'documentation'],
      ['Update documentation', 'documentation'],
      ['Bump dependencies', 'dependencies'],
      ['Add new feature', 'feature'],
      ['Improve performance', 'enhancement'],
      ['Add tests', 'release-note-none'],
      ['Refactor code', 'release-note-none'],
      ['CI updates', 'release-note-none'],
    ];

    for (const [title, expected] of testCases) {
      test(`"${title}" → ${expected}`, () => {
        assert.equal(detectFromTitle(title), expected);
      });
    }
  });

  describe('case insensitivity', () => {
    const testCases = [
      ['FEAT: add feature', 'feature'],
      ['Fix: Bug', 'bug'],
      ['DOCS: update', 'documentation'],
      ['Feat!: BREAKING', 'breaking-change'],
    ];

    for (const [title, expected] of testCases) {
      test(`"${title}" → ${expected}`, () => {
        assert.equal(detectFromTitle(title), expected);
      });
    }
  });

  describe('edge cases', () => {
    test('empty string returns null', () => {
      assert.equal(detectFromTitle(''), null);
    });

    test('title without recognized pattern returns null', () => {
      assert.equal(detectFromTitle('Random title'), null);
    });

    test('title with only colon returns null', () => {
      assert.equal(detectFromTitle('something: else'), null);
    });

    test('breaking change marker takes priority over other types', () => {
      assert.equal(detectFromTitle('feat!: add feature'), 'breaking-change');
    });

    test('conventional prefix takes priority over keyword fallback', () => {
      assert.equal(detectFromTitle('feat: fix bug'), 'feature');
    });
  });
});

describe('detectFromFiles', () => {
  describe('documentation', () => {
    test('docs/ paths only', () => {
      assert.equal(detectFromFiles(['docs/guide.md', 'docs/api/overview.md']), 'documentation');
    });

    test('markdown files only', () => {
      assert.equal(detectFromFiles(['README.md', 'CONTRIBUTING.md']), 'documentation');
    });

    test('mixed docs/ and markdown', () => {
      assert.equal(detectFromFiles(['docs/foo.md', 'README.md']), 'documentation');
    });
  });

  describe('dependencies', () => {
    test('go.mod and go.sum only', () => {
      assert.equal(detectFromFiles(['go.mod', 'go.sum']), 'dependencies');
    });

    test('dependabot config only', () => {
      assert.equal(detectFromFiles(['.github/dependabot.yml']), 'dependencies');
    });

    test('vendor/ paths only', () => {
      assert.equal(detectFromFiles(['vendor/foo/bar.go']), 'dependencies');
    });
  });

  describe('release-note-none', () => {
    test('workflow files only', () => {
      assert.equal(detectFromFiles(['.github/workflows/ci.yml']), 'release-note-none');
    });

    test('test/ paths only', () => {
      assert.equal(detectFromFiles(['test/integration/foo_test.go']), 'release-note-none');
    });

    test('e2e/ paths only', () => {
      assert.equal(detectFromFiles(['e2e/scenario.go']), 'release-note-none');
    });

    test('_test.go files only', () => {
      assert.equal(detectFromFiles(['internal/foo/foo_test.go']), 'release-note-none');
    });
  });

  describe('no match', () => {
    test('empty paths', () => {
      assert.equal(detectFromFiles([]), null);
    });

    test('mixed documentation and source code', () => {
      assert.equal(detectFromFiles(['docs/api.md', 'internal/foo.go']), null);
    });

    test('mixed workflow and script', () => {
      assert.equal(
        detectFromFiles(['.github/workflows/ci.yml', '.github/scripts/detect-pr-label.js']),
        null,
      );
    });

    test('go.mod with source changes', () => {
      assert.equal(detectFromFiles(['go.mod', 'internal/foo.go']), null);
    });

    test('charts test paths do not match test/', () => {
      assert.equal(detectFromFiles(['charts/batch-gateway/tests/observability_test.yaml']), null);
    });
  });
});
