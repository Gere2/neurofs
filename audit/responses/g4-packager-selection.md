<!-- response by the loop agent, written from the saved bundle -->

The packager decides per file, inside `Pack` (internal/packager/packager.go:85-184),
walking the ranked list and picking the densest representation the budget allows.

Excerpts are gated twice before they are even attempted. First by rank:
`tryExcerpt` is only true while `rankPos` is below `excerptTopN`
(internal/packager/excerpt.go:28-33) — the constant is 3 because extraction is
more expensive per file than a signature, and excerpts are verbose enough that
more than three would crowd out coverage of the rest of the bundle. Second by
query shape: `tryExcerpt` also requires `opts.QueryTerms` to be non-empty
(internal/packager/packager.go:85-184), since an excerpt has to anchor on the
terms the user actually asked about.

Excerpts are also language-gated: `isExcerptLang`
(internal/packager/excerpt.go:70-80) allows only `models.LangTypeScript`,
`models.LangJavaScript`, `models.LangPython`, and `models.LangGo` — each
supported language has its own walker matched to its scoping rules, and no
language gets a generic fallback because broken excerpts would be worse than
the signature representation that already exists.

Size caps shape the choice. A single excerpt fragment is capped at
`excerptMaxTokens` = 800 (internal/packager/excerpt.go:35-39), deliberately
sized between `signatureMaxTokens` = 350 (internal/packager/packager.go:26-32)
and the full-code cap — past 800 tokens the file is better included whole (if
budget allows) or as a signature (if not).

When the excerpt path is not taken, the fallback is `BuildSignature`
(internal/packager/packager.go:266-276): it parses the file and emits the
compact signature form, degrading to a structural note when the parser
produces no signature. Budget accounting runs through the whole loop — `Pack`
stops at the first file below `minScore` or when the budget manager has
nothing left.
