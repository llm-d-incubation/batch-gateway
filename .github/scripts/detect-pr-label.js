const PREFIX_TYPE_LABELS = Object.fromEntries(
  Object.entries({
    'breaking-change': ['breaking', 'break'],
    'feature': ['feat', 'feature'],
    'enhancement': ['enh', 'enhancement', 'improve'],
    'bug': ['fix', 'bugfix', 'bug'],
    'documentation': ['docs', 'doc'],
    'dependencies': ['deps', 'dependencies'],
    'release-note-none': ['test', 'refactor', 'ci', 'chore', 'style', 'perf', 'revert'],
  }).flatMap(([label, types]) => types.map((type) => [type, label])),
);

const KEYWORD_LABEL_RULES = [
  ['breaking-change', /\b(breaking|break)\b/],
  ['bug', /\b(fix|bugfix|bug)\b/],
  ['documentation', /\b(docs?|documentation)\b/],
  ['dependencies', /\b(deps|dependencies)\b/],
  ['feature', /\b(feat(?:ure)?)\b/],
  ['enhancement', /\b(enh(?:ancement)?|improve(?:ment)?)\b/],
  ['release-note-none', /\b(tests?|refactor|ci|chore|style|perf|revert)\b/],
];

function detectFromTitle(title) {
  const lower = title.toLowerCase();

  // Priority 1: Breaking change indicators (! marker or breaking keyword in prefix)
  // Handles: "!: description", "type!: description", "type(scope)!: description"
  if (lower.includes('!:') || lower.match(/^(\w+)(?:\([^)]*\))?!:/)) {
    return 'breaking-change';
  }

  // Priority 2: Conventional commit prefix (type: or type(scope):)
  const prefixMatch = lower.match(/^(\w+)(?:\([^)]*\))?:/);
  if (prefixMatch) {
    const label = PREFIX_TYPE_LABELS[prefixMatch[1]];
    if (label) return label;
  }

  // Priority 3: Keyword fallback for titles without conventional commit format
  for (const [label, pattern] of KEYWORD_LABEL_RULES) {
    if (pattern.test(lower)) return label;
  }

  return null;
}

// Labels only when every changed file matches the category (avoids mislabeling mixed PRs).
function detectFromFiles(paths) {
  if (paths.length === 0) return null;

  if (paths.every((p) => p.startsWith('docs/') || p.endsWith('.md'))) {
    return 'documentation';
  }
  if (paths.every((p) =>
    p === 'go.mod' ||
    p === 'go.sum' ||
    p === '.github/dependabot.yml' ||
    p.startsWith('vendor/')
  )) {
    return 'dependencies';
  }
  if (paths.every((p) =>
    p.startsWith('.github/workflows/') ||
    p.startsWith('test/') ||
    p.startsWith('e2e/') ||
    p.endsWith('_test.go')
  )) {
    return 'release-note-none';
  }

  return null;
}

module.exports = { detectFromTitle, detectFromFiles };
