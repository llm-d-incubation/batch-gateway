const TYPE_LABELS = {
  feat: 'feature',
  enh: 'enhancement',
  fix: 'bug',
  docs: 'documentation',
  deps: 'dependencies',
  ci: 'release-note-none',
  chore: 'release-note-none',
  refactor: 'release-note-none',
  test: 'release-note-none',
  style: 'release-note-none',
  perf: 'release-note-none',
  revert: 'release-note-none',
  build: 'release-note-none',
};

const MANAGED_CATEGORY_LABELS = [
  'breaking-change',
  'feature',
  'enhancement',
  'bug',
  'documentation',
  'dependencies',
  'release-note-none',
];

function isBreakingChange(title) {
  const lower = title.toLowerCase();
  return lower.startsWith('!:') || /^(\w+)(?:\([^)]*\))?!:/.test(lower);
}

function labelFromType(type, title) {
  if (isBreakingChange(title)) {
    return 'breaking-change';
  }
  return TYPE_LABELS[type] ?? null;
}

module.exports = { labelFromType, isBreakingChange, MANAGED_CATEGORY_LABELS, TYPE_LABELS };
