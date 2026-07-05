# G5 fixtures — pallets/click (Python lib shape)

Reproducible fact fixtures for the G5 cross-shape check. Each fact is a real
identifier verified to exist in click's `src/`. Run against a fresh clone:

```
git clone --depth 1 https://github.com/pallets/click /tmp/click
neurofs scan /tmp/click
neurofs economy --repo /tmp/click --fixtures-dir docs/g5_fixtures/click
neurofs gate    --repo /tmp/click --fixtures-dir docs/g5_fixtures/click
```

See ../../phase_g5_cross_shape.md for the recorded verdicts.
