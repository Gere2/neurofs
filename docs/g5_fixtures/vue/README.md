# G5 fixtures — vuejs/core (TypeScript frontend shape)

These six fixtures contain grep-verified renderer, compiler, scheduler,
component, computed, and reactivity identifiers. The recorded G5 run pins
commit `b5f8518379b77c3b62a7a9d2b52f6c76cda09bd5`.

```
ENGINE=/path/to/NeuroFS
TARGET=/tmp/vue
REPORT_DIR=/tmp/vue-g5-reports
git clone https://github.com/vuejs/core "$TARGET"
git -C "$TARGET" checkout --detach b5f8518379b77c3b62a7a9d2b52f6c76cda09bd5
mkdir -p "$TARGET/.neurofs" "$REPORT_DIR"
cp "$ENGINE/.neurofs/weights.json" "$TARGET/.neurofs/weights.json"
cd "$ENGINE" && make build
export NEUROFS_EMBEDDING_PROVIDER=mock
unset NEUROFS_MOCK_SEMANTIC
"$ENGINE/bin/neurofs" scan "$TARGET"
"$ENGINE/bin/neurofs" economy --repo "$TARGET" \
  --fixtures-dir "$ENGINE/docs/g5_fixtures/vue" \
  --g5-attest --g5-engine-root "$ENGINE" \
  --out "$REPORT_DIR/typescript_frontend-economy.json"
"$ENGINE/bin/neurofs" gate --repo "$TARGET" \
  --fixtures-dir "$ENGINE/docs/g5_fixtures/vue" \
  --g5-attest --g5-engine-root "$ENGINE" \
  --out "$REPORT_DIR/typescript_frontend-gate.json"
"$ENGINE/bin/neurofs" bench --repo "$TARGET" \
  --file "$ENGINE/docs/g5_bench/vue.json" \
  --out "$REPORT_DIR/typescript_frontend-bench.txt"
```

The attested gate derives G2 from the exact bundles it creates for G3; it is
one pass and deliberately rejects `--bundles-dir`. See
../../phase_g5_cross_shape.md for the recorded verdicts and hashes.
