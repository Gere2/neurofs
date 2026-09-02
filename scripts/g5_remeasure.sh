#!/usr/bin/env bash
# Re-measure the G5 cross-shape evidence for the current branch.
#
# G5 binds the evidence to the engine's source-tree digest, which covers every
# tracked .go/go.mod/go.sum plus every indexed path — including .md docs. So
# any commit touching code, docs, or dependencies invalidates it and the gate
# goes red until this runs. That is working as designed; the friction is only
# that the procedure has two traps, both of which have bitten:
#
#   1. Measure from a CLEAN CLONE, never the working tree. The target digest
#      covers every indexed file, so local-only artifacts poison it and the
#      result verifies on your machine and nowhere else.
#   2. Verify AFTER committing, by re-cloning (scripts/g5_verify.sh). Checking
#      the tree you measured proves nothing about the tree CI will see.
#
# Usage: scripts/g5_remeasure.sh [tag]
set -uo pipefail

REPO="$(git rev-parse --show-toplevel)"
BRANCH="$(git -C "$REPO" rev-parse --abbrev-ref HEAD)"
TAG="${1:-mechanical}"
RUN="$(date -u +%Y-%m-%dT%H%M%SZ)-${TAG}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/neurofs-g5-XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

echo "==> measuring $BRANCH as $RUN"

# Corpora are pinned to the commits the retained evidence used, so the external
# shapes stay comparable across runs and only the Go shape moves.
LATEST="$(ls -1 "$REPO"/audit/g5/*.json 2>/dev/null | sort | tail -1)"
if [ -z "$LATEST" ]; then
  echo "no existing evidence in audit/g5 to read pinned corpus commits from" >&2
  exit 1
fi
pinned() {
  python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(next(s['commit_sha'] for s in d['shapes'] if s['kind']==sys.argv[2]))" "$LATEST" "$1"
}
clone_pinned() {
  local dir="$1" url="$2" sha="$3"
  if [ "$(git -C "$dir" rev-parse HEAD 2>/dev/null)" = "$sha" ]; then return 0; fi
  echo "==> fetching $(basename "$dir") @ ${sha:0:12}"
  rm -rf "$dir"; mkdir -p "$dir"
  git -C "$dir" init -q
  git -C "$dir" remote add origin "$url"
  git -C "$dir" fetch -q --depth 1 origin "$sha" || return 1
  git -C "$dir" checkout -q FETCH_HEAD || return 1
}
clone_pinned /tmp/click https://github.com/pallets/click "$(pinned python_library)" || exit 1
clone_pinned /tmp/vue https://github.com/vuejs/core "$(pinned typescript_frontend)" || exit 1

# Every measured shape is attested against the engine's weights, so each
# corpus needs that exact weights.json on disk — the retained evidence
# records one weights_sha256 shared by all three shapes. Unconditional: a
# corpus already at the pinned SHA skips clone_pinned above and would
# otherwise keep whatever (or nothing) it had. Without this the run dies at
# the first corpus with "hash measured retrieval weights: no such file".
for corpus in /tmp/click /tmp/vue; do
  mkdir -p "$corpus/.neurofs"
  cp "$REPO/.neurofs/weights.json" "$corpus/.neurofs/weights.json"
done

# Trap 1: a clean clone of the branch, not this working tree.
git clone -q --branch "$BRANCH" "$REPO" "$WORK/engine"
cd "$WORK/engine"
git remote set-url origin https://github.com/Gere2/neurofs.git
mkdir -p .neurofs && cp "$REPO/.neurofs/weights.json" .neurofs/weights.json

export NEUROFS_EMBEDDING_PROVIDER=mock
unset NEUROFS_MOCK_SEMANTIC

# `make build` stamps the source digest into the binary; a plain `go build`
# leaves it empty and G5 then fails for an unrelated reason.
make build >/dev/null || { echo "build failed" >&2; exit 1; }

R="audit/g5/reports/$RUN"
mkdir -p "$R"
ENGINE="$PWD"
BIN="$ENGINE/bin/neurofs"

measure() {
  local repo="$1" kind="$2" fixtures="$3" bench="$4"
  echo "==> $kind"
  "$BIN" scan "$repo" >/dev/null || return 1
  "$BIN" economy --repo "$repo" --fixtures-dir "$fixtures" \
    --g5-attest --g5-engine-root "$ENGINE" --out "$R/$kind-economy.json" >/dev/null || return 1
  # The per-shape gate exits non-zero by design — its own G1/G5 are red for a
  # foreign repo — but still writes --out. Only its G2/G3 rows are quoted.
  "$BIN" gate --repo "$repo" --fixtures-dir "$fixtures" \
    --g5-attest --g5-engine-root "$ENGINE" --out "$R/$kind-gate.json" >/dev/null
  [ -s "$R/$kind-gate.json" ] || { echo "$kind: gate wrote no report" >&2; return 1; }
  if [ -n "$bench" ]; then
    "$BIN" bench --repo "$repo" ${bench:+--file "$bench"} --out "$R/$kind-bench.txt" >/dev/null
  else
    "$BIN" bench --repo "$repo" --out "$R/$kind-bench.txt" >/dev/null
  fi
}

measure "$ENGINE" go_service audit/facts "" || exit 1
measure /tmp/click python_library docs/g5_fixtures/click docs/g5_bench/click.json || exit 1
measure /tmp/vue typescript_frontend docs/g5_fixtures/vue docs/g5_bench/vue.json || exit 1

"$BIN" g5-assemble --reports-dir "$R" --out "audit/g5/$RUN.json" || exit 1

mkdir -p "$REPO/audit/g5/reports/$RUN"
cp "audit/g5/$RUN.json" "$REPO/audit/g5/"
cp "$R"/* "$REPO/audit/g5/reports/$RUN/"

python3 - "$REPO/audit/g5/$RUN.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for s in d["shapes"]:
    print(f"  {s['kind']:22} economy {s['economy']['verdict']:4}  "
          f"G2 {s['g2']['verdict']:4}  G3 {s['g3']['verdict']:4} "
          f"{s['g3']['mean_recall']*100:5.1f}%")
PY

cat <<EOF

Evidence written to audit/g5/$RUN.json

  Next, in this order — the verify step is the one that is easy to skip and
  the only one that checks the tree CI will actually see:

    git add audit/g5 && git commit
    scripts/g5_verify.sh
EOF
