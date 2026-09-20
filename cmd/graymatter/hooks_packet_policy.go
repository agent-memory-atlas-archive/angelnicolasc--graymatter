package main

import (
	"fmt"
	"strings"
)

// An empty value means the installed hook follows the product default. An
// explicit native choice stays explicit across reinstalls and future defaults.
func installedHookPacketPolicy(groups any, exe string) (string, error) {
	arr, ok := groups.([]any)
	if !ok {
		return "", nil
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
				return "", err
			}
			if seen && chosen != policy {
				return "", fmt.Errorf("conflicting installed packet policies; specify --packet-policy native or lexical")
			}
			chosen, seen = policy, true
		}
	}
	return chosen, nil
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
	policy, err := installedHookPacketPolicy(groups, exe)
	if err != nil {
		return hookCheck{Name: "packet policy", Status: "fail", Detail: err.Error(), Hint: "reinstall with --packet-policy native or lexical"}
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
