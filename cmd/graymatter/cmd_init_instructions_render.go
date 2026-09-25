package main

import (
	"bytes"
	"errors"
	"strings"
	"unicode/utf8"
)

func renderInitInstructions(data []byte, exists bool) ([]byte, string, error) {
	if exists && (!utf8.Valid(data) || bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf})) {
		return nil, "", errors.New("unsupported_encoding")
	}
	block := instructionsBlock()
	if !exists {
		return []byte(block), "created", nil
	}
	content := string(data)
	if usesCRLF(content) {
		block = toCRLF(block)
	}
	le := lineEnding(content)
	var next string
	if len(data) == 0 {
		next = block
	} else {
		stripped, at := stripManagedBlocks(content)
		if at >= 0 {
			next = stripped[:at] + strings.TrimSuffix(block, le) + stripped[at:]
		} else {
			next = strings.TrimRight(stripped, "\r\n") + le + le + block
		}
	}
	if content == next {
		return data, "unchanged", nil
	}
	return []byte(next), "updated", nil
}
