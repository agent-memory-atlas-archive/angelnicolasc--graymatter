package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"
)

const initConfigLimit = 4 << 20
const initJSONMaxDepth = 1000

type jsonSpan struct{ start, end int }
type jsonMember struct {
	key   string
	value jsonNode
}
type jsonNode struct {
	jsonSpan
	kind     byte
	close    int
	members  []jsonMember
	children []jsonNode
	comma    bool
}

type jsonCursor struct {
	data  []byte
	pos   int
	jsonc bool
	depth int
}

func (p *jsonCursor) space() error {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		case '/':
			if !p.jsonc || p.pos+1 >= len(p.data) {
				return errors.New("unexpected slash")
			}
			switch p.data[p.pos+1] {
			case '/':
				p.pos += 2
				for p.pos < len(p.data) && p.data[p.pos] != '\n' {
					p.pos++
				}
			case '*':
				p.pos += 2
				end := bytes.Index(p.data[p.pos:], []byte("*/"))
				if end < 0 {
					return errors.New("unclosed comment")
				}
				p.pos += end + 2
			default:
				return errors.New("unexpected slash")
			}
		default:
			return nil
		}
	}
	return nil
}

func (p *jsonCursor) string() (string, error) {
	if p.pos >= len(p.data) || p.data[p.pos] != '"' {
		return "", errors.New("expected string")
	}
	start := p.pos
	p.pos++
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == '\\' {
			p.pos += 2
			continue
		}
		p.pos++
		if c == '"' {
			var s string
			if err := json.Unmarshal(p.data[start:p.pos], &s); err != nil {
				return "", err
			}
			return s, nil
		}
	}
	return "", errors.New("unterminated string")
}

func (p *jsonCursor) value() (jsonNode, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > initJSONMaxDepth {
		return jsonNode{}, errors.New("document nesting too deep")
	}
	if err := p.space(); err != nil {
		return jsonNode{}, err
	}
	if p.pos >= len(p.data) {
		return jsonNode{}, errors.New("expected value")
	}
	n := jsonNode{jsonSpan: jsonSpan{start: p.pos}, kind: p.data[p.pos]}
	switch n.kind {
	case '{':
		p.pos++
		seen := map[string]bool{}
		for {
			if err := p.space(); err != nil {
				return n, err
			}
			if p.pos >= len(p.data) {
				return n, errors.New("unterminated object")
			}
			if p.data[p.pos] == '}' {
				n.close = p.pos
				p.pos++
				if n.comma && !p.jsonc {
					return n, errors.New("trailing comma")
				}
				break
			}
			if len(n.members) > 0 && !n.comma {
				return n, errors.New("missing comma")
			}
			key, err := p.string()
			if err != nil {
				return n, err
			}
			if seen[key] {
				return n, errors.New("duplicate key")
			}
			seen[key] = true
			if err := p.space(); err != nil {
				return n, err
			}
			if p.pos >= len(p.data) || p.data[p.pos] != ':' {
				return n, errors.New("expected colon")
			}
			p.pos++
			value, err := p.value()
			if err != nil {
				return n, err
			}
			n.members = append(n.members, jsonMember{key: key, value: value})
			if err := p.space(); err != nil {
				return n, err
			}
			n.comma = p.pos < len(p.data) && p.data[p.pos] == ','
			if n.comma {
				p.pos++
			}
		}
	case '[':
		p.pos++
		count, comma := 0, false
		for {
			if err := p.space(); err != nil {
				return n, err
			}
			if p.pos >= len(p.data) {
				return n, errors.New("unterminated array")
			}
			if p.data[p.pos] == ']' {
				if comma && !p.jsonc {
					return n, errors.New("trailing comma")
				}
				p.pos++
				break
			}
			if count > 0 && !comma {
				return n, errors.New("missing comma")
			}
			child, err := p.value()
			if err != nil {
				return n, err
			}
			n.children = append(n.children, child)
			count++
			if err := p.space(); err != nil {
				return n, err
			}
			comma = p.pos < len(p.data) && p.data[p.pos] == ','
			if comma {
				p.pos++
			}
		}
	case '"':
		if _, err := p.string(); err != nil {
			return n, err
		}
	default:
		start := p.pos
		for p.pos < len(p.data) {
			switch p.data[p.pos] {
			case ' ', '\t', '\r', '\n', ',', ']', '}', '/':
				goto done
			}
			p.pos++
		}
	done:
		if !json.Valid(p.data[start:p.pos]) {
			return n, errors.New("invalid scalar")
		}
	}
	n.end = p.pos
	return n, nil
}

func parseInitJSON(data []byte, jsonc bool) (jsonNode, error) {
	if !utf8.Valid(data) || bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		return jsonNode{}, errors.New("unsupported_encoding")
	}
	p := jsonCursor{data: data, jsonc: jsonc}
	n, err := p.value()
	if err != nil {
		return n, errors.New("invalid_document")
	}
	if err := p.space(); err != nil || p.pos != len(data) {
		return n, errors.New("invalid_document")
	}
	if n.kind != '{' {
		return n, errors.New("invalid_root")
	}
	return n, nil
}

func jsonField(n jsonNode, key string) (jsonNode, bool) {
	for _, m := range n.members {
		if m.key == key {
			return m.value, true
		}
	}
	return jsonNode{}, false
}

func jsonSemantic(n jsonNode, data []byte) any {
	switch n.kind {
	case '{':
		m := make(map[string]any, len(n.members))
		for _, field := range n.members {
			m[field.key] = jsonSemantic(field.value, data)
		}
		return m
	case '[':
		out := make([]any, len(n.children))
		for i, child := range n.children {
			out[i] = jsonSemantic(child, data)
		}
		return out
	default:
		var v any
		_ = json.Unmarshal(data[n.start:n.end], &v)
		return v
	}
}

func jsonInsertMember(data []byte, object jsonNode, key string, value []byte, jsonc bool) ([]byte, error) {
	keyJSON, _ := json.Marshal(key)
	line := []byte("\n")
	if i := bytes.IndexByte(data, '\n'); i > 0 && data[i-1] == '\r' {
		line = []byte("\r\n")
	}
	insert := append(append(append([]byte{}, line...), ' ', ' '), keyJSON...)
	insert = append(insert, ':', ' ')
	insert = append(insert, value...)
	if len(object.members) > 0 && !object.comma {
		// Place the separator directly after the last value, before comments.
		last := object.members[len(object.members)-1].value.end
		data = append(append(append([]byte{}, data[:last]...), ','), data[last:]...)
		object.close++
	}
	insert = append(insert, line...)
	out := make([]byte, 0, len(data)+len(insert))
	out = append(out, data[:object.close]...)
	out = append(out, insert...)
	out = append(out, data[object.close:]...)
	if _, err := parseInitJSON(out, jsonc); err != nil {
		return nil, errors.New("unsupported_edit")
	}
	return out, nil
}

func planJSONMCP(data []byte, exists bool, topKey string, entry map[string]any, jsonc, replace bool) ([]byte, string, error) {
	want, err := json.Marshal(entry)
	if err != nil {
		return nil, "", err
	}
	if !exists {
		root := map[string]any{topKey: map[string]any{"graymatter": entry}}
		if jsonc {
			root["$schema"] = "https://opencode.ai/config.json"
		}
		out, _ := json.MarshalIndent(root, "", "  ")
		return append(out, '\n'), "created", nil
	}
	if len(data) == 0 {
		return nil, "", errors.New("invalid_document")
	}
	root, err := parseInitJSON(data, jsonc)
	if err != nil {
		return nil, "", err
	}
	parent, hasParent := jsonField(root, topKey)
	if !hasParent {
		value := append(append([]byte{'{'}, []byte(strconv.Quote("graymatter")+":")...), want...)
		value = append(value, '}')
		out, err := jsonInsertMember(data, root, topKey, value, jsonc)
		return out, "updated", err
	}
	if parent.kind != '{' {
		return nil, "", errors.New("invalid_parent")
	}
	target, hasTarget := jsonField(parent, "graymatter")
	if !hasTarget {
		out, err := jsonInsertMember(data, parent, "graymatter", want, jsonc)
		return out, "updated", err
	}
	if target.kind != '{' {
		return nil, "", errors.New("invalid_target")
	}
	got, _ := jsonSemantic(target, data).(map[string]any)
	if jsonEqual(got, entry) {
		return data, "unchanged", nil
	}
	if !replace {
		return data, "preserved", nil
	}
	out := append(append(append([]byte{}, data[:target.start]...), want...), data[target.end:]...)
	if _, err := parseInitJSON(out, jsonc); err != nil {
		return nil, "", fmt.Errorf("unsupported_edit")
	}
	return out, "updated", nil
}
