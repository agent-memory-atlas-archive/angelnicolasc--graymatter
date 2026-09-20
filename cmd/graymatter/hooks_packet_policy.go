package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// An empty value means the installed hook follows the product default. An
// explicit native choice stays explicit across reinstalls and future defaults.
func installedHookPacketPolicy(groups any, exe string) (string, error) {
	policy, _, err := configuredHookPacketPolicy(groups, exe)
	return policy, err
}

func configuredHookPacketPolicy(groups any, exe string) (string, bool, error) {
	arr, ok := groups.([]any)
	if !ok {
		return "", false, nil
	}
	chosen := ""
	seen := false
	for _, group := range arr {
		g, _ := group.(map[string]any)
		hooks, _ := g["hooks"].([]any)
		for _, entry := range hooks {
			h, _ := entry.(map[string]any)
			if !hookEntryIsOurs(h, exe) {
				continue
			}
			args, structured := hookEntryArgs(h)
			if !structured {
				command, _ := h["command"].(string)
				_, tail, found := strings.Cut(command, hooksRunCommandMarker)
				if !found {
					continue
				}
				args = strings.Fields(tail)
			}
			policy, err := hookPacketPolicyFromArgs(args)
			if err != nil {
				return "", true, err
			}
			if seen && chosen != policy {
				return "", true, fmt.Errorf("conflicting installed packet policies; specify --packet-policy native or lexical")
			}
			chosen, seen = policy, true
		}
	}
	return chosen, seen, nil
}

func hookPacketPolicyFromArgs(args []string) (string, error) {
	policy := ""
	seen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value, found := strings.CutPrefix(arg, "--packet-policy=")
		if arg == "--packet-policy" {
			found = true
			i++
			if i == len(args) {
				return "", fmt.Errorf("installed packet policy has no value")
			}
			value = args[i]
		}
		if found {
			if seen || !validHookPacketPolicy(value) {
				return "", fmt.Errorf("invalid or duplicate installed packet policy")
			}
			policy, seen = value, true
		}
	}
	return policy, nil
}

func hookPacketPolicyCheck(groups any, exe string) hookCheck {
	policy, found, err := configuredHookPacketPolicy(groups, exe)
	if err != nil {
		return hookCheck{Name: "packet policy", Status: "fail", Detail: err.Error(), Hint: "reinstall with --packet-policy native or lexical"}
	}
	if !found {
		return hookCheck{Name: "packet policy", Status: "info", Detail: "not configured (no managed UserPromptSubmit hook)"}
	}
	if policy == "" {
		return hookCheck{Name: "packet policy", Status: "info", Detail: "native (product default; no explicit policy)"}
	}
	detail := policy + " (explicit installed choice)"
	if policy == "lexical" {
		detail += "; experimental, up to 32 candidates and 3 whole facts / 832 UTF-8 payload bytes per namespace"
	}
	return hookCheck{Name: "packet policy", Status: "info", Detail: detail}
}

// These checks describe only the named settings file, not the host's effective
// configuration or precedence across settings sources. They never open a store.
func hookPacketConfigurationCheck(root map[string]any, path, exe string, scope hookScope) hookCheck {
	var check hookCheck
	hooks, ok := root["hooks"].(map[string]any)
	switch {
	case root == nil:
		check = hookCheck{Name: "packet policy", Status: "fail", Detail: "settings must be a JSON object"}
	case root["hooks"] != nil && !ok:
		check = hookCheck{Name: "packet policy", Status: "fail", Detail: "hooks must be an object"}
	default:
		groups := hooks[hooksEventUserPrompt]
		if _, ok := groups.([]any); groups != nil && !ok {
			check = hookCheck{Name: "packet policy", Status: "fail", Detail: "UserPromptSubmit hooks must be a list"}
		} else {
			check = hookPacketPolicyCheck(groups, exe)
		}
	}
	return scopeHookPacketCheck(check, path, scope)
}

func readHookPacketConfigurationCheck(path, exe string, scope hookScope) hookCheck {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return scopeHookPacketCheck(hookCheck{Name: "packet policy", Status: "info", Detail: "not configured (settings file absent)"}, path, scope)
	}
	if err != nil {
		return scopeHookPacketCheck(hookCheck{Name: "packet policy", Status: "fail", Detail: fmt.Sprintf("cannot read settings: %v", err)}, path, scope)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return scopeHookPacketCheck(hookCheck{Name: "packet policy", Status: "fail", Detail: "settings must be a valid JSON object"}, path, scope)
	}
	return hookPacketConfigurationCheck(root, path, exe, scope)
}

func scopeHookPacketCheck(check hookCheck, path string, scope hookScope) hookCheck {
	if check.Status == "info" && !strings.HasPrefix(check.Detail, "lexical ") {
		check.Hint = "lexical is available as an experimental policy; configure with `graymatter hooks install --scope " + string(scope) + " --packet-policy lexical`"
	} else if check.Status == "fail" {
		check.Hint = "inspect this settings file; after correcting it, choose a policy with `graymatter hooks install --scope " + string(scope) + " --packet-policy native` (or lexical)"
	}
	check.Detail = fmt.Sprintf("%s settings (%s): %s", scope, path, check.Detail)
	return check
}
