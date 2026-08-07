#!/usr/bin/env bash
# Verify the committed G5 evidence the way CI will: from a fresh clone of the
# current branch, running the retrieval-gates job's exact sequence.
#
# This exists because checking the tree you just measured proves nothing. The
# digest covers every indexed path, so committing the evidence — or an
# unrelated doc edit in the same commit — can invalidate an attestation that
# passed moments earlier in the directory it was produced in.
set -uo pipefail

REPO="$(git rev-parse --show-toplevel)"
BRANCH="$(git -C "$REPO" rev-parse --abbrev-ref HEAD)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/neurofs-g5-verify-XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

if [ -n "$(git -C "$REPO" status --porcelain -- audit/g5)" ]; then
  echo "audit/g5 has uncommitted changes — commit the evidence first, then verify." >&2
  exit 1
fi

echo "==> verifying $BRANCH from a fresh clone"
git clone -q --branch "$BRANCH" "$REPO" "$WORK/engine"
cd "$WORK/engine"
mkdir -p .neurofs && cp "$REPO/.neurofs/weights.json" .neurofs/weights.json

export NEUROFS_EMBEDDING_PROVIDER=mock LANG=C.UTF-8 TZ=UTC
unset NEUROFS_MOCK_SEMANTIC

make check-retrieval check-economy >/dev/null || { echo "retrieval/economy gates failed" >&2; exit 1; }
make scan-self >/dev/null || exit 1

# Written outside the checkout on purpose: an unindexed .json in the repo root
# makes the gate report a stale index, so capturing the report inside the tree
# is itself enough to fail the run.
REPORT="$WORK/gate-report.json"
"$PWD/bin/neurofs" gate --json > "$REPORT"
status=$?

python3 - "$REPORT" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for c in d["criteria"]:
    print(f"  {c['id']}  {c['verdict']:5} {str(c.get('detail',''))[:96]}")
print(f"  overall: {d['overall']}")
PY

if [ $status -ne 0 ]; then
  echo "
gate exited $status — the committed evidence does not verify against this tree." >&2
  exit $status
fi
echo "
Evidence verifies from a clean clone. CI will see the same tree."
