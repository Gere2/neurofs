# G5 fixtures — pallets/click (Python lib shape)

Reproducible fact fixtures for the G5 cross-shape check. Each fact is a real
identifier verified to exist in click's `src/`. The recorded run pins commit
`00e592cea702e0b2caa0dee42489fdb1c22cd845`:

```
ENGINE=/path/to/NeuroFS
TARGET=/tmp/click
REPORT_DIR=/tmp/click-g5-reports
git clone https://github.com/pallets/click "$TARGET"
git -C "$TARGET" checkout --detach 00e592cea702e0b2caa0dee42489fdb1c22cd845
mkdir -p "$TARGET/.neurofs" "$REPORT_DIR"
cp "$ENGINE/.neurofs/weights.json" "$TARGET/.neurofs/weights.json"
cd "$ENGINE" && make build
export NEUROFS_EMBEDDING_PROVIDER=mock
unset NEUROFS_MOCK_SEMANTIC
"$ENGINE/bin/neurofs" scan "$TARGET"
"$ENGINE/bin/neurofs" economy --repo "$TARGET" \
  --fixtures-dir "$ENGINE/docs/g5_fixtures/click" \
  --g5-attest --g5-engine-root "$ENGINE" \
  --out "$REPORT_DIR/python_library-economy.json"
"$ENGINE/bin/neurofs" gate --repo "$TARGET" \
  --fixtures-dir "$ENGINE/docs/g5_fixtures/click" \
  --g5-attest --g5-engine-root "$ENGINE" \
  --out "$REPORT_DIR/python_library-gate.json"
"$ENGINE/bin/neurofs" bench --repo "$TARGET" \
  --file "$ENGINE/docs/g5_bench/click.json" \
  --out "$REPORT_DIR/python_library-bench.txt"
```

The attested gate derives G2 from the exact bundles it creates for G3; it is
one pass and deliberately rejects `--bundles-dir`. See
../../phase_g5_cross_shape.md for the recorded verdicts and hashes.
