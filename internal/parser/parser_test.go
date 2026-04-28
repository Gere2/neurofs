package parser_test

import (
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/parser"
)

func TestParseTypeScript(t *testing.T) {
	content := `
import jwt from 'jsonwebtoken'
import { Request } from 'express'

export interface AuthPayload {
  userId: string
}

export function generateToken(payload: AuthPayload): string {
  return jwt.sign(payload, 'secret')
}

export class AuthMiddleware {
  authenticate() {}
}

export const MAX_RETRIES = 3
`

	result := parser.Parse(models.LangTypeScript, content)

	assertContainsSymbol(t, result.Symbols, "generateToken", "export_func")
	assertContainsSymbol(t, result.Symbols, "AuthMiddleware", "export_class")
	assertContainsSymbol(t, result.Symbols, "AuthPayload", "export_type")
	assertContainsSymbol(t, result.Symbols, "MAX_RETRIES", "export_const")

	assertContainsImport(t, result.Imports, "jsonwebtoken")
	assertContainsImport(t, result.Imports, "express")

	if result.Signature == "" {
		t.Error("expected non-empty signature")
	}
}

func TestParseTypeScriptClassMethods(t *testing.T) {
	content := `
export class AuthMiddleware {
  private userRepo: UserRepository

  constructor(userRepo: UserRepository) {
    this.userRepo = userRepo
  }

  authenticate = async (req: Request, res: Response) => {
    if (req.headers.authorization) {
      const token = 'x'
    }
  }

  async validateToken(token: string): Promise<boolean> {
    return true
  }

  get currentUser() {
    return this._user
  }

  set currentUser(u: User) {
    this._user = u
  }

  private helper() {}
}

class InternalHelper {
  doThing() {}
}
`

	result := parser.Parse(models.LangTypeScript, content)

	// Traditional method.
	assertContainsSymbol(t, result.Symbols, "AuthMiddleware.validateToken", "method")
	// Arrow-function field.
	assertContainsSymbol(t, result.Symbols, "AuthMiddleware.authenticate", "method")
	// Constructor is treated as a method.
	assertContainsSymbol(t, result.Symbols, "AuthMiddleware.constructor", "method")
	// Private method.
	assertContainsSymbol(t, result.Symbols, "AuthMiddleware.helper", "method")
	// Getter / setter.
	assertContainsSymbol(t, result.Symbols, "AuthMiddleware.currentUser", "get")
	assertContainsSymbol(t, result.Symbols, "AuthMiddleware.currentUser", "set")
	// Method on a non-exported class is also captured.
	assertContainsSymbol(t, result.Symbols, "InternalHelper.doThing", "method")

	// Control-flow inside method bodies must not be captured as methods.
	for _, s := range result.Symbols {
		if strings.HasSuffix(s.Name, ".if") || strings.HasSuffix(s.Name, ".for") {
			t.Errorf("control-flow token captured as method: %+v", s)
		}
	}
}

func TestParsePython(t *testing.T) {
	content := `
import hashlib
from typing import Optional, Dict

def compute_checksum(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()

async def fetch_data(url: str) -> Optional[Dict]:
    pass

class EventBus:
    def subscribe(self, event: str) -> None:
        pass
`

	result := parser.Parse(models.LangPython, content)

	assertContainsSymbol(t, result.Symbols, "compute_checksum", "func")
	assertContainsSymbol(t, result.Symbols, "fetch_data", "func")
	assertContainsSymbol(t, result.Symbols, "EventBus", "class")

	assertContainsImport(t, result.Imports, "hashlib")
	assertContainsImport(t, result.Imports, "typing")
}

func TestParseGo(t *testing.T) {
	content := `
package storage

import (
	"database/sql"
	"fmt"
)

type DB struct {
	db *sql.DB
}

func Open(path string) (*DB, error) {
	return nil, nil
}

func (s *DB) Close() error {
	return nil
}

const DefaultTimeout = 30

var ErrNotFound = fmt.Errorf("not found")
`

	result := parser.Parse(models.LangGo, content)

	assertContainsSymbol(t, result.Symbols, "DB", "type")
	assertContainsSymbol(t, result.Symbols, "Open", "func")
	assertContainsSymbol(t, result.Symbols, "DefaultTimeout", "const")
	assertContainsSymbol(t, result.Symbols, "ErrNotFound", "var")

	assertContainsImport(t, result.Imports, "database/sql")
	assertContainsImport(t, result.Imports, "fmt")
}

// TestParseGoExtractsParenthesisedConstAndVarBlocks pins the recovery
// of grouped const/var specs that the single-line regex misses. This
// is the regression test for G3 facts RepExcerpt and fullCodeMaxTokens
// — both lived in `const ( ... )` blocks and were therefore invisible
// to the index, which made them invisible in signatures and to the
// ranker's symbol_match signal.
func TestParseGoExtractsParenthesisedConstAndVarBlocks(t *testing.T) {
	content := `
package models

const (
	// RepFullCode includes the complete file content.
	RepFullCode Representation = "full_code"
	RepExcerpt   Representation = "excerpt"
	RepSignature Representation = "signature"
)

const (
	fullCodeMaxTokens           = 600
	aggressiveFullCodeMaxTokens = 180
)

var (
	defaultBudget = 8000
	knownPaths, knownAPIs []string
)

const SoloConst = 1
var SoloVar = 2
`
	result := parser.Parse(models.LangGo, content)

	consts := []string{
		"RepFullCode", "RepExcerpt", "RepSignature",
		"fullCodeMaxTokens", "aggressiveFullCodeMaxTokens",
		"SoloConst", // single-line const still works
	}
	vars := []string{
		"defaultBudget", "knownPaths", "knownAPIs",
		"SoloVar", // single-line var still works
	}
	for _, want := range consts {
		assertContainsSymbol(t, result.Symbols, want, "const")
	}
	for _, want := range vars {
		assertContainsSymbol(t, result.Symbols, want, "var")
	}

	// Signature must surface the const names so files with grouped consts
	// stop disappearing from the index. Spot-check a few.
	for _, want := range []string{
		"const RepExcerpt = ...",
		"const fullCodeMaxTokens = ...",
		"var defaultBudget ...",
	} {
		if !strings.Contains(result.Signature, want) {
			t.Errorf("signature missing %q\n---\n%s", want, result.Signature)
		}
	}
}

// TestParseGoBlockSpecsSkipNoise confirms blank lines, comment-only
// lines, and the `_` blank identifier do not appear as symbols.
func TestParseGoBlockSpecsSkipNoise(t *testing.T) {
	content := `
package x

const (
	// section header

	A = 1

	/* inline comment */
	B = 2
	_ = "ignored"
)
`
	result := parser.Parse(models.LangGo, content)
	assertContainsSymbol(t, result.Symbols, "A", "const")
	assertContainsSymbol(t, result.Symbols, "B", "const")
	for _, name := range []string{"_", "section", "header", "inline", "comment"} {
		for _, s := range result.Symbols {
			if s.Name == name {
				t.Errorf("noise leaked into symbols: %q", name)
			}
		}
	}
}

func TestParseMarkdown(t *testing.T) {
	content := `
# Architecture Overview

## Authentication Layer

### JWT Strategy

Some content here.

## Data Layer
`

	result := parser.Parse(models.LangMarkdown, content)

	assertContainsSymbol(t, result.Symbols, "Architecture Overview", "h1")
	assertContainsSymbol(t, result.Symbols, "Authentication Layer", "h2")
	assertContainsSymbol(t, result.Symbols, "JWT Strategy", "h3")
	assertContainsSymbol(t, result.Symbols, "Data Layer", "h2")
}

func TestParseUnknown(t *testing.T) {
	result := parser.Parse(models.LangUnknown, "anything")
	if len(result.Symbols) != 0 || len(result.Imports) != 0 {
		t.Error("expected empty result for unknown language")
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func assertContainsSymbol(t *testing.T, syms []models.Symbol, name, kind string) {
	t.Helper()
	for _, s := range syms {
		if s.Name == name && s.Kind == kind {
			return
		}
	}
	t.Errorf("symbol %q (kind=%q) not found in %v", name, kind, syms)
}

func assertContainsImport(t *testing.T, imports []string, want string) {
	t.Helper()
	for _, imp := range imports {
		if imp == want {
			return
		}
	}
	t.Errorf("import %q not found in %v", want, imports)
}
