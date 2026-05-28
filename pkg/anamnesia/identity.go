package anamnesia

// Identity is the boot-shaped view of who the user is. Persona drives
// voice ("how to respond"); Profile is biographical fact ("who they are").
// SystemPrompt is the rendered block dock-on agents put at the top of
// their system message.
type Identity struct {
	Scope        Scope          `json:"scope"`
	Persona      map[string]any `json:"persona"`
	Profile      map[string]any `json:"profile"`
	SystemPrompt string         `json:"system_prompt"`
}
