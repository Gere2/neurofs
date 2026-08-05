package receipt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// DecodeRecord decodes one JSONL line strictly and canonically. Strict
// decoding is part of the integrity story: any representation the content
// hash cannot see must be rejected, because the hash covers the re-encoded
// typed struct, not the bytes that were parsed.
//
// Three layers close the known gaps of encoding/json:
//
//   - DisallowUnknownFields rejects fields the schema does not define.
//   - A second Decode must hit clean io.EOF — Decoder.More() tolerates a
//     trailing ']' or '}', an explicit EOF check does not.
//   - The input must equal json.Marshal of the decoded record byte-for-byte.
//     The ledger's encoding is canonical (writers emit json.Marshal of this
//     struct), so this eliminates duplicate keys (last one wins), keys with
//     alternative capitalization (encoding/json matches case-insensitively),
//     null overwrites of typed fields, and every whitespace variant —
//     including outer padding, which is why data is compared as given and
//     never trimmed.
//
// Forward compatibility is governed by schema_version, not by ignoring
// unknown or ambiguous input.
func DecodeRecord(data []byte) (Record, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Record
	if err := dec.Decode(&r); err != nil {
		return Record{}, fmt.Errorf("receipt: decode: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return Record{}, fmt.Errorf("receipt: trailing data after record")
	}
	canonical, err := json.Marshal(r)
	if err != nil {
		return Record{}, fmt.Errorf("receipt: re-encode: %w", err)
	}
	if !bytes.Equal(data, canonical) {
		return Record{}, fmt.Errorf("receipt: line is not the canonical encoding of its record")
	}
	return r, nil
}
