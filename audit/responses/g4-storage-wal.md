<!-- response by the loop agent, written from the saved bundle -->

`Open` (internal/storage/storage.go:115-153) creates the database directory
with `os.MkdirAll`, then opens the index via `sql.Open` with the `sqlite`
driver. Connection setup is a fixed pragma sequence, and the order matters:
`PRAGMA busy_timeout = 5000` runs before `PRAGMA journal_mode = WAL` so the
WAL switch itself waits when another process is mid-switch. The full sequence
is `busy_timeout`, `journal_mode = WAL`, `synchronous = NORMAL` (the
documented safe pair for WAL), and `foreign_keys = ON`. Without these, two
concurrent `neurofs scan` invocations collide instantly with `SQLITE_BUSY`.

Each pragma is executed through `execWithBusyRetry`
(internal/storage/storage.go:115-153), and busy detection is centralised in
`isSQLiteBusy` (internal/storage/storage.go:199-207), which matches the
lowercased error text against `sqlite_busy`, `database is locked`, and
`database table is locked` — the three shapes the driver actually produces.

WAL is what lets readers proceed during writes; the index stays readable while
a scan rewrites it. Maintenance paths follow the same discipline: for example
`DB.DeleteRemovedFiles` (internal/storage/storage.go:380-434) runs its deletes
inside a single transaction so an error partway through rolls back, keeping
the reported "Removed" count and the on-disk index from diverging on failure.
