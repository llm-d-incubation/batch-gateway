#!/bin/bash
# Tests for generate-release.sh. Stubs the gh CLI on PATH and asserts which
# git/refs POSTs the script makes (or that it bails before any gh call).
# No bats or other harness required — plain bash. Run via: make test-scripts
#
# Covers the release-driving logic that performs irreversible remote writes:
#   - final release (vX.Y.Z)        -> branch + tag
#   - pre-release   (vX.Y.Z-<pre>)  -> tag only, no branch
#   - abbreviated SHA is resolved to the full 40-char SHA before create-ref
#   - malformed SHA/version bail before any gh api call

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${SCRIPT_DIR}/generate-release.sh"

RESOLVED_SHA="0123456789abcdef0123456789abcdef01234567"

FAILURES=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; FAILURES=$((FAILURES + 1)); }

# Build a throwaway dir holding a fake gh whose api invocations are logged to
# $GH_LOG. Prepending it to PATH shadows the real gh for the script under test.
# Ref state lives in $STUB_DIR/refs ("<ref> <sha>" per line): a create-ref POST
# appends to it, and a git/ref GET reads from it (404 if absent), so the stub
# models ref existence across calls. Pre-seed it to simulate a prior partial run.
setup_stub() {
    STUB_DIR="$(mktemp -d)"
    GH_LOG="${STUB_DIR}/gh.log"
    : >"$GH_LOG"
    : >"${STUB_DIR}/refs"
    cat >"${STUB_DIR}/gh" <<STUB
#!/bin/bash
echo "\$*" >>"${GH_LOG}"
[ "\$1" = "auth" ] && exit 0
[ "\$1" = "api" ] || exit 0
refs="${STUB_DIR}/refs"
ref=""; sha=""
for a in "\$@"; do
    case "\$a" in
        ref=*) ref="\${a#ref=}" ;;
        sha=*) sha="\${a#sha=}" ;;
    esac
done
if [ "\$3" = "--method" ] && [ "\$4" = "POST" ]; then
    echo "\${ref} \${sha}" >>"\$refs"
    exit 0
fi
case "\$2" in
    */commits/*) echo "${RESOLVED_SHA}"; exit 0 ;;
    */git/ref/*)
        want="refs/\${2#*/git/ref/}"
        line="\$(grep "^\${want} " "\$refs" 2>/dev/null | head -1)"
        [ -n "\$line" ] && { echo "\${line#* }"; exit 0; }
        exit 1
        ;;
esac
exit 0
STUB
    chmod +x "${STUB_DIR}/gh"
}

teardown_stub() { rm -rf "$STUB_DIR"; }

# invoke <sha> <version> ; requires an active stub; sets $STATUS.
invoke() {
    PATH="${STUB_DIR}:${PATH}" "$TARGET" "$1" "$2" >/dev/null 2>&1
    STATUS=$?
}

# run_script <sha> <version> ; fresh stub, then invoke.
run_script() {
    setup_stub
    invoke "$1" "$2"
}

# --- final release: branch + tag, both with the resolved full SHA ---
run_script "$RESOLVED_SHA" "v1.2.3"
if [ "$STATUS" -eq 0 ] \
    && grep -q "git/refs --method POST -f ref=refs/heads/release-v1.2.3 -f sha=${RESOLVED_SHA}" "$GH_LOG" \
    && grep -q "git/refs --method POST -f ref=refs/tags/v1.2.3 -f sha=${RESOLVED_SHA}" "$GH_LOG"; then
    pass "final release v1.2.3 creates branch + tag on resolved SHA"
else
    fail "final release v1.2.3 creates branch + tag on resolved SHA"
fi
teardown_stub

# --- pre-release: tag only, no branch ---
run_script "$RESOLVED_SHA" "v1.2.3-rc1"
if [ "$STATUS" -eq 0 ] \
    && grep -q "git/refs --method POST -f ref=refs/tags/v1.2.3-rc1 -f sha=${RESOLVED_SHA}" "$GH_LOG" \
    && ! grep -q "ref=refs/heads/" "$GH_LOG"; then
    pass "pre-release v1.2.3-rc1 creates tag only, no branch"
else
    fail "pre-release v1.2.3-rc1 creates tag only, no branch"
fi
teardown_stub

# --- abbreviated SHA is resolved before create-ref (the 422-avoidance fix) ---
run_script "abc1234" "v1.2.3"
if [ "$STATUS" -eq 0 ] \
    && grep -q "sha=${RESOLVED_SHA}" "$GH_LOG" \
    && ! grep -q "sha=abc1234" "$GH_LOG"; then
    pass "abbreviated SHA resolved to full SHA for create-ref"
else
    fail "abbreviated SHA resolved to full SHA for create-ref"
fi
teardown_stub

# --- malformed SHA bails before any gh api call ---
run_script "nothexsha!" "v1.2.3"
if [ "$STATUS" -ne 0 ] && ! grep -q '^api' "$GH_LOG"; then
    pass "malformed SHA bails before any gh api call"
else
    fail "malformed SHA bails before any gh api call"
fi
teardown_stub

# --- malformed version bails before any gh api call ---
run_script "$RESOLVED_SHA" "1.2"
if [ "$STATUS" -ne 0 ] && ! grep -q '^api' "$GH_LOG"; then
    pass "malformed version bails before any gh api call"
else
    fail "malformed version bails before any gh api call"
fi
teardown_stub

# --- re-run after a partial failure: existing branch at target SHA is reused,
#     tag still gets created (recovery is safe, no manual branch delete) ---
setup_stub
echo "refs/heads/release-v1.2.3 ${RESOLVED_SHA}" >"${STUB_DIR}/refs"
invoke "$RESOLVED_SHA" "v1.2.3"
if [ "$STATUS" -eq 0 ] \
    && ! grep -q "method POST -f ref=refs/heads/release-v1.2.3" "$GH_LOG" \
    && grep -q "method POST -f ref=refs/tags/v1.2.3 -f sha=${RESOLVED_SHA}" "$GH_LOG"; then
    pass "re-run reuses existing branch and creates the tag"
else
    fail "re-run reuses existing branch and creates the tag"
fi
teardown_stub

# --- existing ref at a different SHA is an error, not a silent move ---
setup_stub
echo "refs/heads/release-v1.2.3 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" >"${STUB_DIR}/refs"
invoke "$RESOLVED_SHA" "v1.2.3"
if [ "$STATUS" -ne 0 ] && ! grep -q "method POST" "$GH_LOG"; then
    pass "existing ref at a different SHA errors before any create-ref"
else
    fail "existing ref at a different SHA errors before any create-ref"
fi
teardown_stub

echo ""
if [ "$FAILURES" -eq 0 ]; then
    echo "✅ All generate-release.sh tests passed!"
    exit 0
fi
echo "❌ ${FAILURES} generate-release.sh test(s) failed"
exit 1
