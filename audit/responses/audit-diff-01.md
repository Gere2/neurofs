### 1. Diseño técnico

- **Función pura** `audit.DiffRecords(a, b AuditRecord) Diff` en `internal/audit/diff.go`. Sin I/O, fácil de testear.
- **Struct `Diff`** con:
  - Metadata: `SameBundle`, `SameQuestion`, `SameModel` (booleans para que el CLI decida si avisar).
  - Scalar deltas: `GroundedDelta`, `DriftDelta`, `RecallDelta` (B − A), más `RecallApplies` para no mostrar recall cuando ninguno de los records tenía facts.
  - Tres `SetDiff{Added, Removed}`: uno por bucket (`Paths`, `APIs`, `Symbols`). `Added = in B, not in A`; `Removed = in A, not in B`.
- **Convención de signo**: positivos = B más alto que A. Así "grounded +10%" y "drift -5%" son ambas buenas noticias; el usuario interpreta por nombre, no por signo.
- **CLI**: `neurofs audit diff <a.json> <b.json>` con `--json <path>` opcional. Nueva subcomando colgado del parent `audit`. No toca `replay`.
- **Bundle hash distinto** no es error — es exactamente el caso "reindexé entre corridas"; lo marcamos visible pero seguimos.

### 2. Archivos a tocar
- [internal/audit/diff.go](internal/audit/diff.go) — nuevo.
- [internal/audit/diff_test.go](internal/audit/diff_test.go) — nuevo.
- [internal/cli/audit.go](internal/cli/audit.go) — añadir `newAuditDiffCmd()` + `printDiffSummary()`; registrar bajo el parent.

### 3. Código

Ahora el subcomando CLI.

All green. Ahora el demo: necesito un segundo record para diff. Reuso el bundle stemming y genero una segunda respuesta con menos alucinaciones.

### 4. Cómo probarlo

### 5. Ejemplo de salida CLI

Diff funciona. Lectura correcta: respuesta B perdió grounding (-16.7) pero redujo drift (-7.1) y eliminó 3 símbolos alucinados (`internal/ranking/deep_stem.go`, `path_match`, `symbol_match`) — cambio mixto que el reviewer puede juzgar con datos concretos.

Comprobemos también la salida JSON:

Todo funciona end-to-end.

**Resumen final**

- [internal/audit/diff.go](internal/audit/diff.go) — `DiffRecords` puro + `SetDiff`/`Diff` structs.
- [internal/audit/diff_test.go](internal/audit/diff_test.go) — 5 tests cubriendo scalars, set diffs, recall skip, bundle mismatch, empty bucket.
- [internal/cli/audit.go](internal/cli/audit.go:33) — nuevo subcomando `diff` registrado bajo `audit`, con `--json` opcional.
- Tests: `go test ./... → all green`. Build limpio.
- Demo: comparar dos records del mismo bundle mostró los deltas correctamente y un diff en JSON parseable.

Sin tocar `replay`, sin inventar estado nuevo, sin superficies adicionales.