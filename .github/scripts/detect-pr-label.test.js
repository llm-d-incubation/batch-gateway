const { describe, test } = require('node:test');
const assert = require('node:assert/strict');
const { labelFromType, isBreakingChange } = require('./detect-pr-label.js');

describe('labelFromType', () => {
  const testCases = [
    ['feat', 'feat: add feature', 'feature'],
    ['enh', 'enh: improve performance', 'enhancement'],
    ['fix', 'fix: bug', 'bug'],
    ['docs', 'docs: update', 'documentation'],
    ['deps', 'deps(go): bump', 'dependencies'],
    ['ci', 'ci: update workflow', 'release-note-none'],
    ['chore', 'chore: misc', 'release-note-none'],
    ['refactor', 'refactor: cleanup', 'release-note-none'],
    ['test', 'test: add tests', 'release-note-none'],
    ['style', 'style: format', 'release-note-none'],
    ['perf', 'perf: optimize', 'release-note-none'],
    ['revert', 'revert: undo change', 'release-note-none'],
    ['build', 'build: update makefile', 'release-note-none'],
    ['feat', 'feat(api): add endpoint', 'feature'],
    ['fix', 'fix(auth): resolve bug', 'bug'],
    ['feat', 'feat!: breaking feature', 'breaking-change'],
    ['fix', 'fix!: breaking fix', 'breaking-change'],
    ['feat', 'feat(api)!: breaking change', 'breaking-change'],
    ['feat', '!: breaking change', 'breaking-change'],
    ['fix', 'fix: handle error when status is !: done', 'bug'],
    ['unknown', 'unknown: something', null],
  ];

  for (const [type, title, expected] of testCases) {
    test(`type=${type} title="${title}" → ${expected}`, () => {
      assert.equal(labelFromType(type, title), expected);
    });
  }
});

describe('isBreakingChange', () => {
  test('detects prefix !:', () => {
    assert.equal(isBreakingChange('!: breaking'), true);
  });

  test('detects type!:', () => {
    assert.equal(isBreakingChange('feat!: breaking'), true);
  });

  test('ignores !: in subject', () => {
    assert.equal(isBreakingChange('fix: handle error when status is !: done'), false);
  });
});
