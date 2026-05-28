package store

import (
	"fmt"
	"sort"
	"strings"

	"github.com/flohs/anamnesia-open-source/pkg/anamnesia"
)

// RenderIdentity produces the deterministic system-prompt block that
// every dock-on agent puts at the top of its system message. It is the
// single source of truth for "how to talk to this user".
//
// Order:
//  1. user.persona.system_prompt verbatim (if present)
//  2. other persona keys as "<key>: <value>" lines, sorted
//  3. separator
//  4. "### About me" + profile keys as "- <key>: <value>" bullets, sorted
//
// Empty input produces empty output.
func RenderIdentity(id anamnesia.Identity) string {
	if len(id.Persona) == 0 && len(id.Profile) == 0 {
		return ""
	}
	var b strings.Builder
	if sp, ok := id.Persona["system_prompt"]; ok {
		if s, ok := sp.(string); ok && strings.TrimSpace(s) != "" {
			b.WriteString(strings.TrimSpace(s))
			b.WriteString("\n")
		}
	}
	otherPersonaKeys := make([]string, 0, len(id.Persona))
	for k := range id.Persona {
		if k == "system_prompt" {
			continue
		}
		otherPersonaKeys = append(otherPersonaKeys, k)
	}
	sort.Strings(otherPersonaKeys)
	for _, k := range otherPersonaKeys {
		fmt.Fprintf(&b, "%s: %v\n", k, id.Persona[k])
	}
	if len(id.Profile) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("### About me\n")
		profileKeys := make([]string, 0, len(id.Profile))
		for k := range id.Profile {
			profileKeys = append(profileKeys, k)
		}
		sort.Strings(profileKeys)
		for _, k := range profileKeys {
			fmt.Fprintf(&b, "- %s: %v\n", k, id.Profile[k])
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
