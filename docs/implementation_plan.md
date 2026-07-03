# NeuroFS - Plan de implementacion estrategica

Ultima revision: 2026-05-27

Este plan aterriza el radar competitivo de `docs/strategic_anticipations.md` en cambios tecnicos incrementales. Parte del estado real observado en el repo: ya existen `neurofs watch`, `fsnotify`, limpieza incremental de ficheros borrados, grafo de relaciones, herramientas MCP quirurgicas y extraccion AST para excerpts de Go.

## Estado actual

Ya presente en el codigo:

- `internal/indexer/watcher.go`: file watcher recursivo con debounce y actualizacion incremental de registros.
- `internal/cli/watch.go`: comando `neurofs watch`.
- `internal/storage/storage.go`: `DeleteFile`, `file_embeddings`, `file_relations` y metodos de relaciones.
- `internal/indexer/graph.go`: relaciones por imports locales y coincidencias de paquetes.
- `internal/indexer/chunker.go`: chunker Go basado en `go/parser`, fallback de fichero completo y embeddings por `content_hash`.
- `internal/mcp/tools.go`: `neurofs_context`, `neurofs_task`, `neurofs_scan`, `neurofs_view_file`, `neurofs_get_outline`, `neurofs_list_signatures`, `neurofs_get_excerpt`, `neurofs_search`.
- `internal/packager/excerpt_go.go`: camino AST con `go/parser` para excerpts Go.
- `internal/retrieval/search.go`: capa compartida de busqueda por chunks usada por MCP, CLI, benchmarks y taskflow.

Brechas principales:

- Ranking y packing siguen soportando registros de fichero, pero `pack --chunks` y `task --chunks` ya pueden saltar directamente a excerpts por rangos de linea.
- El watcher reindexa el fichero completo cuando cambia cualquier bloque, aunque los embeddings por chunk ya se cachean por `content_hash`.
- TS/JS/Python siguen usando parsing heuristico para simbolos y excerpts.
- MCP ya expone un primer broker/router (`neurofs_context`), pero aun falta hacerlo mas fino por perfiles, costes y salida progresiva.
- No hay ledger de sesiones ni memoria portable tipo Chronicle/AGENTS/CLAUDE.

## Fase 5A - Living Index v2: chunks, hashes y cache parcial

Objetivo: que NeuroFS actualice solo los bloques que cambiaron y pueda responder a agentes con rangos de codigo exactos.

Estado: primer slice implementado para Go y fallback de fichero completo para el resto de lenguajes.

Cambios propuestos:

1. [x] Anadir tabla `chunks`.

```sql
CREATE TABLE IF NOT EXISTS chunks (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    file_path     TEXT NOT NULL,
    chunk_id      TEXT NOT NULL,
    parent_id     TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL,
    symbol        TEXT NOT NULL DEFAULT '',
    start_line    INTEGER NOT NULL,
    end_line      INTEGER NOT NULL,
    content_hash  TEXT NOT NULL,
    ast_hash      TEXT NOT NULL DEFAULT '',
    token_estimate INTEGER NOT NULL DEFAULT 0,
    indexed_at    TEXT NOT NULL,
    UNIQUE(file_path, chunk_id),
    FOREIGN KEY(file_path) REFERENCES files(path) ON DELETE CASCADE
);
```

2. [x] Anadir tabla `chunk_embeddings`.

```sql
CREATE TABLE IF NOT EXISTS chunk_embeddings (
    content_hash TEXT PRIMARY KEY,
    embedding    BLOB NOT NULL,
    provider     TEXT NOT NULL,
    model        TEXT NOT NULL,
    created_at   TEXT NOT NULL
);
```

3. [x] Crear un `Chunker` interno:

```go
type Chunk struct {
    FilePath      string
    ChunkID       string
    ParentID      string
    Kind          string
    Symbol        string
    StartLine     int
    EndLine       int
    Content       string
    ContentHash   string
    ASTHash       string
    TokenEstimate int
}

type Chunker interface {
    Chunk(path string, lang models.Lang, content string) ([]Chunk, error)
}
```

4. [x] Cambiar `indexer.Run` y `Watcher` para:
   - calcular chunks despues de `parser.Parse`;
   - borrar chunks obsoletos del fichero;
   - upsert solo chunks vivos;
   - pedir embeddings solo para `content_hash` nuevos;
   - mantener `files.checksum` como hash de fichero completo.

Pruebas:

- [x] Un cambio dentro de una funcion cambia solo el hash del chunk afectado.
- [x] `go test ./...` pasa con chunks activados.
- [x] Un rename/delete debe limpiar `files`, `chunks`, `file_embeddings` y `chunk_embeddings` no referenciados.
- [x] Dos runs sobre el mismo repo deben producir el mismo orden y los mismos hashes en un fixture dedicado.

## Fase 5B - AST multi-lenguaje

Objetivo: igualar o superar el chunking sintactico de IDEs propietarios sin encerrar el indice en una nube.

Orden recomendado:

1. **Go**: consolidar el camino existente de `go/parser` para que tambien alimente `chunks`, no solo excerpts.
2. **TypeScript/JavaScript**: integrar Tree-sitter o un parser externo rapido detras de una interfaz. Mantener fallback heuristico.
3. **Python**: usar parser AST cuando sea viable; fallback por indentacion.
4. **Rust/Java/C++**: empezar por top-level symbols y line ranges, despues scopes internos.

Criterios de aceptacion:

- Cada chunk tiene line range cerrado y no solapado salvo relacion parent-child.
- Las funciones/metodos/clases principales aparecen como chunks independientes.
- Si el parser AST falla, NeuroFS degrada a heuristicas actuales y marca `kind=heuristic`.
- `neurofs_get_excerpt` puede servirse desde `chunks` sin volver a parsear el fichero completo.

## Fase 6A - Retrieval hibrido y busqueda agentica rapida

Objetivo: competir con Fast Context/SWE-grep desde una version local y auditable.

Estado: noveno slice implementado con scoring lexical/symbol/path/content sobre chunks persistidos, `rg` exacto para identificadores, match exacto de nombres de fichero, similitud por embeddings cacheados, expansion por grafo de imports directos, boost por working set de git, penalizacion de chunks largos cuando existen alternativas pequenas, medicion CLI con `bench --search`, bundle directo por chunks via `neurofs pack --chunks` y prompt one-shot por chunks via `neurofs task --chunks`. Pendiente: decidir si `neurofs_task` debe usar search como preselector por defecto sin flag.

Herramienta MCP:

```json
{
  "name": "neurofs_search",
  "description": "Return ranked code spans for a query using lexical, semantic, symbol, graph and git signals.",
  "input": {
    "query": "string",
    "repo": "string",
    "limit": "integer",
    "mode": "research|build|review|test"
  }
}
```

Salida esperada:

```json
{
  "results": [
    {
      "path": "internal/packager/excerpt_go.go",
      "start_line": 40,
      "end_line": 96,
      "symbol": "extractGoExcerpt",
      "score": 13.4,
      "reasons": ["symbol_match", "semantic_match", "graph_neighbor"],
      "snippet": "..."
    }
  ]
}
```

Ranking hibrido:

- [x] Chunks persistidos con line ranges y snippets.
- [x] Coincidencia lexical por contenido.
- [x] Coincidencia por simbolo, path y kind.
- [x] Embeddings por chunk para sinonimia.
- [x] Grafo para expandir dependencias directas.
- [x] Git changes para priorizar el working set.
- [x] `rg` exacto para terminos, identificadores y nombres de fichero.
- [ ] SQLite symbols/imports para saltos estructurales.
- [x] Penalizacion por chunks largos si existen alternativas mas pequenas.

Metricas:

- [x] Recall de facts contra snippets devueltos por `neurofs_search`.
- [x] Tokens devueltos por pregunta en `neurofs bench --search`.
- [x] Latencia p50/p95 en `neurofs bench --search`.
- [x] Estabilidad: misma query + mismo indice => mismo prefijo JSON (`--search-stability`).
- [x] Ratio `search/bundle tokens` cuando se combina `--search --bundle`.
- [x] Variante de bundle por chunks (`neurofs pack --chunks`).
- [x] Variante one-shot por chunks (`neurofs task --chunks`) con excerpts citables y cache separada.

## Fase 6B - MCP broker y dieta de herramientas

Objetivo: anticipar la direccion de deferred tool loading y reducir tool bloat.

Estado: cuarto slice implementado. `neurofs_context` acepta `intent` (`outline`, `search`, `excerpt`, `bundle`, `build`, `research`, `review`, `test` o `unknown`), infiere ruta cuando no hay intent claro, devuelve `tool_trace`, expone `structural_hints` desde simbolos/imports persistidos en SQLite, promueve consultas que nombran simbolos a `excerpt`, aplica boost estructural a los resultados de search, usa `task --chunks` para bundles de implementacion, ya se mide desde `neurofs bench --context` y tiene perfiles iniciales con limites/presupuestos diferenciados.

Nueva herramienta de alto nivel:

```json
{
  "name": "neurofs_context",
  "description": "Route a codebase question to the smallest sufficient NeuroFS operation.",
  "input": {
    "query": "string",
    "repo": "string",
    "intent": "outline|search|excerpt|bundle|build|research|review|test|unknown",
    "budget": "integer"
  }
}
```

Comportamiento:

- Si la query es amplia: devuelve outline + top directories.
- Si nombra simbolos: detecta `symbol_matches` desde SQLite y llama search/excerpt.
- Si pide implementacion: devuelve bundle Claude-ready.
- Si pide review: combina changed files + graph neighbors + test hints.
- Siempre devuelve `tool_trace` con suboperaciones, tokens y razones; cuando aplica, devuelve `structural_hints` con paths, simbolos/imports coincidentes y score.

Mantener las herramientas atomicas existentes para clientes avanzados, pero recomendar `neurofs_context` como entrada por defecto.

## Fase 7 - Ledger de sesiones y memoria portable

Objetivo: que NeuroFS recuerde trabajo real sin depender de la memoria propietaria de un IDE.

Archivo propuesto: `.neurofs/ledger.jsonl`.

Evento minimo:

```json
{
  "ts": "2026-05-27T10:15:00Z",
  "session_id": "local-...",
  "query": "review current edits",
  "bundle_hash": "...",
  "files": ["internal/ranking/ranking.go"],
  "commands": ["go test ./internal/ranking"],
  "outcome": "pass",
  "notes": ["ranking boost touched dependency graph"]
}
```

Exportadores:

- `neurofs memory export --format agents` => bloque para `AGENTS.md`.
- `neurofs memory export --format claude` => bloque para `CLAUDE.md`.
- `neurofs memory search "ranking boost"` => eventos relevantes con paths y fechas.

Regla: la memoria nunca debe ser opaca. Cada entrada debe tener origen, fecha, archivos y razon de inclusion.

## Fase 8 - Federacion privada opcional

Objetivo: ofrecer una alternativa abierta a indices cloud compartidos.

Primer diseno:

- Calcular `repo_simhash` desde hashes de chunks.
- Permitir cache remota opcional de embeddings por `content_hash`, no por path ni contenido.
- Para equipos, compartir solo hashes y embeddings de chunks aprobados.
- Exigir prueba local de posesion: el cliente solo puede recibir resultados para hashes que ya tiene.

No implementar hasta que Fase 5A y 6A sean solidas. La federacion sin chunks confiables complica la seguridad demasiado pronto.

## Corte inmediato recomendado

El primer slice de Fase 5A, los cortes principales de `neurofs_search`, `pack --chunks`, `task --chunks` y los cuatro primeros slices de `neurofs_context` ya estan implementados. El siguiente corte recomendado es **endurecer la decision de default**:

1. Usar `bench --context` como gate de precision/tokens para los perfiles.
2. Medir `task --chunks` frente a `task` en los fixtures existentes.
3. Decidir si `neurofs_task` debe usar search como preselector por defecto sin flag.
4. Extender chunking AST a TS/JS para que `pack --chunks` no dependa de fallback de fichero completo.

Este corte convierte el indice vivo en una superficie real para agentes, no solo en metadata preparada para el futuro.
