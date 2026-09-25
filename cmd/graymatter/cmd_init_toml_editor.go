package main

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

type tomlSection struct {
	start int
	end   int
	path  []string
}

func tomlHeaderPath(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "[") || strings.HasPrefix(line, "[[") {
		return nil, false
	}
	end := strings.IndexByte(line, ']')
	if end < 0 {
		return nil, false
	}
	tail := strings.TrimSpace(line[end+1:])
	if tail != "" && !strings.HasPrefix(tail, "#") {
		return nil, false
	}
	body := line[1:end]
	var parts []string
	for len(body) > 0 {
		body = strings.TrimLeft(body, " \t")
		if body == "" {
			return nil, false
		}
		var part string
		switch body[0] {
		case '"', '\'':
			quote := body[0]
			i := 1
			for i < len(body) {
				if body[i] == '\\' && quote == '"' {
					i += 2
					continue
				}
				if body[i] == quote {
					break
				}
				i++
			}
			if i >= len(body) {
				return nil, false
			}
			part = body[1:i]
			if quote == '"' {
				decoded, err := strconv.Unquote(body[:i+1])
				if err != nil {
					return nil, false
				}
				part = decoded
			}
			body = body[i+1:]
		default:
			i := strings.IndexByte(body, '.')
			if i < 0 {
				i = len(body)
			}
			part = strings.TrimSpace(body[:i])
			body = body[i:]
		}
		if part == "" {
			return nil, false
		}
		parts = append(parts, part)
		body = strings.TrimLeft(body, " \t")
		if body == "" {
			break
		}
		if body[0] != '.' {
			return nil, false
		}
		body = body[1:]
	}
	return parts, len(parts) > 0
}

// tomlSections locates table headers while ignoring headers embedded in TOML
// multiline strings. The semantic parser validates the entire document first.
func tomlSections(data []byte) []tomlSection {
	var sections []tomlSection
	mode := ""
	for start := 0; start < len(data); {
		end := bytes.IndexByte(data[start:], '\n')
		if end < 0 {
			end = len(data)
		} else {
			end += start + 1
		}
		line := string(data[start:end])
		if mode == "" {
			if path, ok := tomlHeaderPath(line); ok {
				if len(sections) > 0 {
					sections[len(sections)-1].end = start
				}
				sections = append(sections, tomlSection{start: start, end: len(data), path: path})
			}
		}
		for i := 0; i < len(line); i++ {
			if mode != "" {
				if strings.HasPrefix(line[i:], mode) &&
					(mode == "'''" || !tomlEscapedQuote(line, i)) {
					i += 2
					mode = ""
				}
				continue
			}
			if line[i] == '#' {
				break
			}
			if strings.HasPrefix(line[i:], `"""`) || strings.HasPrefix(line[i:], "'''") {
				mode = line[i : i+3]
				i += 2
				continue
			}
			if line[i] == '"' || line[i] == '\'' {
				q := line[i]
				i++
				for i < len(line) {
					if line[i] == '\\' && q == '"' {
						i += 2
						continue
					}
					if line[i] == q {
						break
					}
					i++
				}
			}
		}
		start = end
	}
	return sections
}

func tomlEscapedQuote(line string, at int) bool {
	backslashes := 0
	for i := at - 1; i >= 0 && line[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 != 0
}

func tomlPathPrefix(path []string, prefix ...string) bool {
	if len(path) < len(prefix) {
		return false
	}
	return reflect.DeepEqual(path[:len(prefix)], prefix)
}

func planTOMLMCP(data []byte, exists bool, selection storeSelection, replace bool) ([]byte, string, error) {
	args := mcpServeArgs(selection)
	want := map[string]any{"command": "graymatter", "args": args}
	argParts := make([]string, len(args))
	for i, arg := range args {
		argParts[i] = strconv.Quote(arg)
	}
	canonical := "[mcp_servers.graymatter]\ncommand = \"graymatter\"\nargs = [" + strings.Join(argParts, ", ") + "]\n"
	if !exists {
		return []byte(canonical), "created", nil
	}
	if len(data) == 0 {
		return nil, "", errors.New("invalid_document")
	}
	if !utf8.Valid(data) || bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		return nil, "", errors.New("unsupported_encoding")
	}
	var root map[string]any
	if _, err := toml.Decode(string(data), &root); err != nil {
		return nil, "", errors.New("invalid_document")
	}
	parent, hasParent := root["mcp_servers"]
	servers, ok := parent.(map[string]any)
	if hasParent && !ok {
		return nil, "", errors.New("invalid_parent")
	}
	sections := tomlSections(data)
	var target []tomlSection
	parentEditable := !hasParent
	for _, sec := range sections {
		if tomlPathPrefix(sec.path, "mcp_servers") {
			parentEditable = true
		}
		if tomlPathPrefix(sec.path, "mcp_servers", "graymatter") {
			target = append(target, sec)
		}
	}
	if got, hasTarget := servers["graymatter"]; hasTarget {
		gm, ok := got.(map[string]any)
		if !ok {
			return nil, "", errors.New("invalid_target")
		}
		if jsonEqual(gm, want) {
			return data, "unchanged", nil
		}
		if !replace {
			return data, "preserved", nil
		}
		if len(target) == 0 {
			return nil, "", errors.New("unsupported_edit")
		}
		// Remove only GrayMatter table sections. Other tables keep their exact
		// bytes, including comments and line endings.
		var out []byte
		prev := 0
		for i, sec := range target {
			out = append(out, data[prev:sec.start]...)
			if i == 0 {
				out = append(out, canonical...)
			}
			prev = sec.end
		}
		out = append(out, data[prev:]...)
		var check map[string]any
		if _, err := toml.Decode(string(out), &check); err != nil {
			return nil, "", errors.New("unsupported_edit")
		}
		return out, "updated", nil
	}
	if !parentEditable {
		return nil, "", errors.New("unsupported_edit")
	}
	eol := "\n"
	if i := bytes.IndexByte(data, '\n'); i > 0 && data[i-1] == '\r' {
		eol = "\r\n"
	}
	appendix := strings.ReplaceAll(canonical, "\n", eol)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		appendix = eol + appendix
	}
	out := append(append([]byte{}, data...), []byte(appendix)...)
	var check map[string]any
	if _, err := toml.Decode(string(out), &check); err != nil {
		return nil, "", fmt.Errorf("unsupported_edit")
	}
	return out, "updated", nil
}
