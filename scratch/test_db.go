package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	tmpDir, err := os.MkdirTemp("", "sqlite-test")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE IF NOT EXISTS session_ledger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp TEXT NOT NULL,
		session_id TEXT NOT NULL,
		query TEXT
	);`
	if _, err := db.Exec(schema); err != nil {
		log.Fatal(err)
	}

	// Insert old entry
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	_, err = db.Exec(`
		INSERT INTO session_ledger (timestamp, session_id, query)
		VALUES (?, ?, ?)
	`, oldTime.UTC().Format(time.RFC3339), "sess-1", "old query")
	if err != nil {
		log.Fatal(err)
	}

	// Insert new entry
	newTime := time.Now().Add(-1 * time.Hour)
	_, err = db.Exec(`
		INSERT INTO session_ledger (timestamp, session_id, query)
		VALUES (?, ?, ?)
	`, newTime.UTC().Format(time.RFC3339), "sess-1", "new query")
	if err != nil {
		log.Fatal(err)
	}

	// Read all
	rows, err := db.Query("SELECT timestamp, query FROM session_ledger")
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var ts, q string
		rows.Scan(&ts, &q)
		fmt.Printf("Row: ts=%s, query=%s\n", ts, q)
	}
	rows.Close()

	// Prune
	cutoff := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	fmt.Printf("Cutoff: %s\n", cutoff)

	res, err := db.Exec("DELETE FROM session_ledger WHERE timestamp < ?", cutoff)
	if err != nil {
		log.Fatal(err)
	}
	affected, _ := res.RowsAffected()
	fmt.Printf("Rows deleted: %d\n", affected)
}
