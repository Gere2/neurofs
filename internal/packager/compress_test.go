package packager

import (
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

func TestStripLicenseHeader(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "C-style block comment license",
			input: `/*
 * Copyright (C) 2026 NeuroFS Authors. All rights reserved.
 * Licensed under the Apache License, Version 2.0.
 */
package main

import "fmt"
`,
			expected: `package main

import "fmt"`,
		},
		{
			name: "Go-style line comment license",
			input: `// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main
`,
			expected: "package main",
		},
		{
			name: "Python-style line comment license",
			input: `# SPDX-License-Identifier: MIT
# Copyright (c) 2026 Jane Doe <jane@example.com>

def hello():
    pass
`,
			expected: `def hello():
    pass`,
		},
		{
			name: "Keep normal comments",
			input: `// This is a normal comment explaining the function
func hello() {
	// Inline comment
}
`,
			expected: `// This is a normal comment explaining the function
func hello() {
	// Inline comment
}`,
		},
		{
			name: "Keep docstring comments",
			input: `/*
Hello performs a greeting.
*/
func Hello() {}
`,
			expected: `/*
Hello performs a greeting.
*/
func Hello() {}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripLicenseHeader(tc.input)
			if got != tc.expected {
				t.Errorf("stripLicenseHeader mismatch\n got:  %q\n want: %q", got, tc.expected)
			}
		})
	}
}

func TestCollapseBlankLines(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "Normalizes newlines and trailing spaces",
			input: "package main \r\n\r\n\r\n\r\nimport \"fmt\"    \n",
			expected: `package main

import "fmt"`,
		},
		{
			name: "Collapses multiple blank lines",
			input: `func hello() {
	println("1")




	println("2")
}`,
			expected: `func hello() {
	println("1")

	println("2")
}`,
		},
		{
			name:     "Trims edges",
			input:    "\n\n\nfunc hello() {}\n\n\n",
			expected: "func hello() {}",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := collapseBlankLines(tc.input)
			if got != tc.expected {
				t.Errorf("collapseBlankLines mismatch\n got:  %q\n want: %q", got, tc.expected)
			}
		})
	}
}

func TestCollapseAllBlankLines(t *testing.T) {
	input := `package main

import "fmt"

func main() {
	println("hello")

	println("world")
}`
	expected := `package main
import "fmt"
func main() {
	println("hello")
	println("world")
}`
	got := collapseAllBlankLines(input)
	if got != expected {
		t.Errorf("collapseAllBlankLines mismatch\n got:  %q\n want: %q", got, expected)
	}
}

func TestCompressIndentation(t *testing.T) {
	tests := []struct {
		name     string
		lang     models.Lang
		input    string
		expected string
	}{
		{
			name: "Compress 4 spaces to 2 spaces",
			lang: models.LangTypeScript,
			input: `class Hello {
    constructor() {
        console.log("hello");
    }
}`,
			expected: `class Hello {
  constructor() {
    console.log("hello");
  }
}`,
		},
		{
			name: "Compress 2 spaces to 1 space",
			lang: models.LangTypeScript,
			input: `class Hello {
  constructor() {
    console.log("hello");
  }
}`,
			expected: `class Hello {
 constructor() {
  console.log("hello");
 }
}`,
		},
		{
			name: "Compress 6 spaces to 3 spaces",
			lang: models.LangTypeScript,
			input: `class Hello {
      constructor() {
            console.log("hello");
      }
}`,
			expected: `class Hello {
   constructor() {
      console.log("hello");
   }
}`,
		},
		{
			name: "Compress tabs to 2 spaces",
			lang: models.LangGo,
			input: `func main() {
	if true {
		println("yes")
	}
}`,
			expected: `func main() {
  if true {
    println("yes")
  }
}`,
		},
		{
			name: "Skip compression for Python",
			lang: models.LangPython,
			input: `def main():
    if True:
        print("yes")`,
			expected: `def main():
    if True:
        print("yes")`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := compressIndentation(tc.lang, tc.input)
			if got != tc.expected {
				t.Errorf("compressIndentation mismatch for %s\n got:  %q\n want: %q", tc.name, got, tc.expected)
			}
		})
	}
}

func TestCompressJSON(t *testing.T) {
	input := `{
		"name": "neurofs",
		"version": "1.0.0",
		"dependencies": {
			"lodash": "^4.17.21"
		}
	}`
	expected := `{"name":"neurofs","version":"1.0.0","dependencies":{"lodash":"^4.17.21"}}`
	got, err := compressJSON(input)
	if err != nil {
		t.Fatalf("compressJSON error: %v", err)
	}
	if got != expected {
		t.Errorf("compressJSON mismatch\n got:  %s\n want: %s", got, expected)
	}
}

func TestStripAllComments(t *testing.T) {
	tests := []struct {
		name     string
		lang     models.Lang
		input    string
		expected string
	}{
		{
			name: "Go line and block comments",
			lang: models.LangGo,
			input: `package main
// line comment
func main() {
	/* block comment */
	s := "http://url.com" // inline comment
	println(s)
}`,
			expected: `package main

func main() {
	
	s := "http://url.com" 
	println(s)
}`,
		},
		{
			name: "Go directives preserved",
			lang: models.LangGo,
			input: `package main
//go:embed file.txt
// +build !dev
// normal comment
func main() {}`,
			expected: `package main
//go:embed file.txt
// +build !dev

func main() {}`,
		},
		{
			name: "Python line comment and triple quote preservation",
			lang: models.LangPython,
			input: `def hello():
    # this is a comment
    x = "url # is not a comment"
    """this is a docstring triple double"""
    y = 'hello'
    '''this is a docstring triple single'''
    return x`,
			expected: `def hello():
    
    x = "url # is not a comment"
    """this is a docstring triple double"""
    y = 'hello'
    '''this is a docstring triple single'''
    return x`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripAllComments(tc.lang, tc.input)
			if got != tc.expected {
				t.Errorf("stripAllComments mismatch for %s\n got:  %q\n want: %q", tc.name, got, tc.expected)
			}
		})
	}
}

func TestCompressCode(t *testing.T) {
	input := `// Copyright 2026 NeuroFS. All rights reserved.
// Apache 2.0 license.

package main

import (
	"fmt"
)



func main() {
	// print hello
	fmt.Println("hello")
}
`
	expected := `package main

import (
  "fmt"
)

func main() {
  // print hello
  fmt.Println("hello")
}`

	got := CompressCode(models.LangGo, input, false, false)
	if got != expected {
		t.Errorf("CompressCode mismatch\n got:  %q\n want: %q", got, expected)
	}

	// Test with stripComments = true, stripBlankLines = true
	expectedCompressed := `package main
import (
  "fmt"
)
func main() {
  fmt.Println("hello")
}`
	gotCompressed := CompressCode(models.LangGo, input, true, true)
	if gotCompressed != expectedCompressed {
		t.Errorf("CompressCode with strip option mismatch\n got:  %q\n want: %q", gotCompressed, expectedCompressed)
	}
}

