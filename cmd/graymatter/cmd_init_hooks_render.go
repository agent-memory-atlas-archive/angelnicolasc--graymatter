package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
)

// renderInitHookSettings edits only the hooks/events it manages. Existing
// settings and foreign matcher groups retain their original bytes, including
// numbers that cannot make a lossless round trip through float64.
func renderInitHookSettings(data []byte, exists bool, exe string, selection storeSelection) ([]byte, string, error) {
	if !exists {
		data = []byte("{}")
	} else if len(data) == 0 {
		return nil, "", errors.New("invalid_document")
	}
	root, err := parseInitJSON(data, false)
	if err != nil {
		return nil, "", err
	}
	hooks, present := jsonField(root, "hooks")
	if present && hooks.kind != '{' {
		return nil, "", errors.New("invalid_parent")
	}
	var existingPrompt any
	if present {
		if prompt, ok := jsonField(hooks, hooksEventUserPrompt); ok {
			existingPrompt = jsonSemantic(prompt, data)
		}
	}
	policy, err := installedHookPacketPolicy(existingPrompt, exe)
	if err != nil {
		return nil, "", errors.New("invalid_hook_policy")
	}
	changed := false
	if !present {
		data, err = jsonInsertMember(data, root, "hooks", []byte("{}"), false)
		if err != nil {
			return nil, "", err
		}
		changed = true
	}
	for _, event := range hookEventNames() {
		root, err = parseInitJSON(data, false)
		if err != nil {
			return nil, "", err
		}
		hooks, _ = jsonField(root, "hooks")
		eventNode, eventPresent := jsonField(hooks, event)
		want := hookGroupsForPolicySelection(exe, event, scopeProject, policy, selection)
		var existing any
		if eventPresent {
			if eventNode.kind != '[' {
				return nil, "", errors.New("unsupported_edit")
			}
			existing = jsonSemantic(eventNode, data)
		}
		_, mutated, _, managed := rewriteHookEvent(existing, want, true, exe)
		if !managed {
			return nil, "", errors.New("unsupported_edit")
		}
		if !mutated {
			continue
		}
		var groups [][]byte
		for _, group := range want {
			encoded, err := json.Marshal(group)
			if err != nil {
				return nil, "", errors.New("unsupported_edit")
			}
			groups = append(groups, encoded)
		}
		if eventPresent {
			value, err := renderInitHookArray(data, eventNode, exe, groups)
			if err != nil {
				return nil, "", err
			}
			data = append(append(append([]byte{}, data[:eventNode.start]...), value...), data[eventNode.end:]...)
		} else {
			value := append(append([]byte{'['}, bytes.Join(groups, []byte(","))...), ']')
			data, err = jsonInsertMember(data, hooks, event, value, false)
			if err != nil {
				return nil, "", err
			}
		}
		if _, err := parseInitJSON(data, false); err != nil {
			return nil, "", errors.New("unsupported_edit")
		}
		changed = true
	}
	if !changed {
		return data, "unchanged", nil
	}
	if !exists {
		data = append(data, '\n')
		return data, "created", nil
	}
	return data, "updated", nil
}

func renderInitHookArray(data []byte, event jsonNode, exe string, groups [][]byte) ([]byte, error) {
	owned := make([]bool, len(event.children))
	lastForeign := -1
	for i, child := range event.children {
		owned[i] = hookGroupIsOurs(jsonSemantic(child, data), exe)
		if !owned[i] {
			lastForeign = i
		}
	}
	if lastForeign < 0 {
		return append(append([]byte{'['}, bytes.Join(groups, []byte(","))...), ']'), nil
	}
	var removed []jsonSpan
	commaBetween := func(left, right int) (jsonSpan, error) {
		start, end := event.children[left].end, event.children[right].start
		i := bytes.IndexByte(data[start:end], ',')
		if i < 0 {
			return jsonSpan{}, errors.New("unsupported_edit")
		}
		return jsonSpan{start: start + i, end: start + i + 1}, nil
	}
	for i := 0; i < len(owned); {
		if !owned[i] {
			i++
			continue
		}
		start := i
		for i < len(owned) && owned[i] {
			removed = append(removed, event.children[i].jsonSpan)
			i++
		}
		end := i - 1
		for j := start; j < end; j++ {
			comma, err := commaBetween(j, j+1)
			if err != nil {
				return nil, err
			}
			removed = append(removed, comma)
		}
		if start > 0 {
			comma, err := commaBetween(start-1, start)
			if err != nil {
				return nil, err
			}
			removed = append(removed, comma)
		} else if i < len(owned) {
			comma, err := commaBetween(end, i)
			if err != nil {
				return nil, err
			}
			removed = append(removed, comma)
		}
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i].start < removed[j].start })
	insert := event.children[lastForeign].end - event.start
	for _, span := range removed {
		if span.end <= event.children[lastForeign].end {
			insert -= span.end - span.start
		}
	}
	array := append([]byte{}, data[event.start:event.end]...)
	for i := len(removed) - 1; i >= 0; i-- {
		span := removed[i]
		start, end := span.start-event.start, span.end-event.start
		array = append(array[:start], array[end:]...)
	}
	if len(groups) == 0 {
		return array, nil
	}
	addition := append([]byte{','}, bytes.Join(groups, []byte(","))...)
	return append(append(append([]byte{}, array[:insert]...), addition...), array[insert:]...), nil
}
