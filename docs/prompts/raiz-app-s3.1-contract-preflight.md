# Raíz App — precondición contractual de S3.1

Trabaja en /Users/gere/raiz-app.

PRECONDICIÓN OBLIGATORIA

Antes de tocar nada verifica:

- rama exacta: fase0-seguridad
- HEAD exacto: 6f2652a
- git status --short contiene únicamente:
  ?? prompt-codex-review.md

Si no coincide, detente y reporta. No añadas, modifiques ni borres
prompt-codex-review.md.

Lee primero AGENT_STATUS.md. Después usa NeuroFS para recuperar únicamente los
fragmentos necesarios de:

- docs/RAIZ-VS-ENVERDE.md
- AGENT_DECISIONS.md
- docs/INVENTORY-REMEDIATION-SPRINTS.md
- docs/AUTOMATIZACION-FACTURAS-STOCK-POS-TESORERIA.md

Sigue íntegramente AGENTS.md y CLAUDE.md.

OBJETIVO

Realiza la precondición contractual de S3.1. No implementes todavía el parser,
los escritores ni los campos de stock. Hay contradicciones que deben quedar
resueltas antes de escribir código.

CONTRADICCIONES QUE DEBES CONTRASTAR EN EL ÁRBOL REAL

1. post-manual-movement.ts, post-waste.ts y post-count.ts ya escriben
   schemaVersion: 2, pero no cumplen el esquema v2 de S3.1: usan qty,
   previousStock, newStock y el vocabulario legacy.

2. El parser prometido dice que schemaVersion === 2 debe seleccionar
   incondicionalmente el parser v2. Aplicarlo hoy convertiría esos movimientos
   existentes en documentos ilegibles.

3. El campo type tiene dos contratos incompatibles:

   - la interfaz y wire.ts usan entrada|salida|merma|ajuste;
   - S3.1 propone
     sale_consumption|sale_reversal|waste|count_adjustment|manual_adjustment.

   No es posible dual-writear ambos significados bajo el mismo nombre.

4. S3.1 enumera cinco tipos, pero
   docs/AUTOMATIZACION-FACTURAS-STOCK-POS-TESORERIA.md incluye ocho:

   opening_balance
   purchase_receipt
   sale_consumption
   sale_reversal
   waste
   count_adjustment
   supplier_return
   manual_adjustment

5. Falta especificar cómo se inicializan currentStockMilli, ledgerVersion y
   projectedLedgerVersion al encontrar un inventory_stock legacy legible pero
   sin esos campos.

TRABAJO

Haz un censo exacto y focalizado de:

- todos los escritores org-scoped de inventory_movements;
- todos los lectores de schemaVersion, type, ledgerType, qty, previousStock y
  newStock;
- todos los escritores y lectores de inventory_stock;
- fixtures que hoy declaran schemaVersion: 2 con forma incompleta;
- scripts que duplican el parser TypeScript.

No confundas la colección top-level legacy del POS con
orgs/{orgId}/inventory_movements.

Entrega una propuesta contractual única que resuelva explícitamente:

A. significado de schemaVersion frente a identityVersion;
B. enum canónico completo de movimientos;
C. cómo preservar el contrato HTTP/UI legacy sin reutilizar type con dos
significados;
D. regla exacta de selección entre parser v2 y parser legacy;
E. bootstrap fail-closed de stock legacy a milli y versiones;
F. qué campos legacy se mantendrán temporalmente y cuáles no pueden
dual-writearse;
G. tratamiento de unitCostMicrosSnapshot y costAmountCents cuando el coste no
sea demostrable;
H. división recomendada de S3.1 en lotes pequeños y revisables, con rutas y
pruebas exactas para cada lote.

Incluye una tabla productor → forma escrita → lectores afectados → migración
propuesta.

No modifiques ningún fichero en este turno. No ejecutes Firebase ni emuladores:
es una inspección estática y documental. Puedes ejecutar pruebas puras
focalizadas solo si sirven para demostrar una contradicción existente.

Al final reporta:

- rama, HEAD y git status;
- archivos inspeccionados;
- contradicciones confirmadas o refutadas con líneas;
- contrato recomendado;
- split exacto de S3.1;
- primer lote implementable después de la decisión.

Detente sin stage, commit, push, deploy, Firebase, reglas ni migraciones. No
empieces S2.2b, S3.2, S3.3 ni S3.4.
