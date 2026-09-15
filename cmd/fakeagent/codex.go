package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func runCodex(args []string, promptReader io.Reader, scenario *Scenario) int {
	prompt, err := extractCodexPrompt(args, promptReader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: codex prompt: %v\n", err)
		return 1
	}
	logInvocation("codex", prompt, args)

	action := scenario.Match(prompt)
	if err := applyAction(action); err != nil {
		return 1
	}

	// Real codex constrains output to --output-schema, so the fake
	// mirrors that by trimming the scenario's catch-all structured map
	// to only fields declared in the schema. Otherwise no-mistakes'
	// schema validation rejects the extra fields the defaultScenario
	// carries to satisfy other steps (e.g. pr's title/body).
	if schemaPath := extractCodexOutputSchema(args); schemaPath != "" && action.Structured != nil {
		filtered, err := filterStructuredToSchema(action.Structured, schemaPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: codex schema filter: %v\n", err)
			return 1
		}
		action.Structured = filtered
	}

	// Replay recorded codex output if a fixture is available. no-mistakes
	// passes a schema file for structured calls, but Codex still surfaces
	// the final answer as agent_message text, so the fixture patches that
	// message body directly.
	flavour := "structured"
	if !action.hasStructuredOutput() && action.Text != "" {
		flavour = "plain"
	}
	if data, err := readFixtureFile(fixtureDir("codex"), flavour, ".jsonl"); err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: codex fixture: %v\n", err)
		return 1
	} else if data != nil {
		patched, err := patchCodexFixture(data, action)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: codex patch: %v\n", err)
			return 1
		}
		os.Stdout.Write(patched)
		return 0
	}

	// Structured Codex output is still delivered as agent_message text.
	// Emit JSON there when requested, otherwise emit the human text.
	body := action.textOrDefault()
	if action.hasStructuredOutput() {
		body = string(action.structuredJSON())
	}

	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"type": "agent_message",
			"text": body,
		},
	})
	_ = enc.Encode(map[string]any{
		"type": "turn.completed",
		"usage": map[string]int{
			"input_tokens":        100,
			"cached_input_tokens": 0,
			"output_tokens":       50,
		},
	})
	return 0
}

// patchCodexFixture rewrites the agent_message item's text body to
// match the scenario action. The wire envelope (thread.started,
// turn.started, item.completed shape, turn.completed.usage) stays
// real. no-mistakes parses JSON out of the agent_message text, so for
// structured responses we substitute the scenario JSON.
func patchCodexFixture(raw []byte, action Action) ([]byte, error) {
	body := action.textOrDefault()
	if action.hasStructuredOutput() {
		body = string(action.structuredJSON())
	}
	var out bytes.Buffer
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 {
			out.WriteByte('\n')
			continue
		}
		var probe struct {
			Type string `json:"type"`
			Item *struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if err := json.Unmarshal(line, &probe); err != nil ||
			probe.Type != "item.completed" ||
			probe.Item == nil ||
			probe.Item.Type != "agent_message" {
			out.Write(line)
			out.WriteByte('\n')
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("parse item event: %w", err)
		}
		item, _ := event["item"].(map[string]any)
		if item != nil {
			item["text"] = body
			event["item"] = item
		}
		patched, err := json.Marshal(event)
		if err != nil {
			return nil, fmt.Errorf("marshal patched item: %w", err)
		}
		out.Write(patched)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// extractCodexOutputSchema returns the --output-schema value from the
// argv, supporting both `--output-schema path` and `--output-schema=path`.
// Returns "" when the flag is absent.
func extractCodexOutputSchema(args []string) string {
	for i, a := range args {
		switch {
		case a == "--output-schema" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "--output-schema="):
			return strings.TrimPrefix(a, "--output-schema=")
		}
	}
	return ""
}

// filterStructuredToSchema makes a canned payload look like real codex output
// for the schema at schemaPath: it drops fields the schema does not declare,
// and supplies null for a declared, nullable field the payload omits.
//
// Both halves mirror what the codex adapter does to the schema it sends
// (internal/agent/codex.go codexOutputSchema): it sets
// additionalProperties:false, so an undeclared field is rejected, and it
// rewrites EVERY declared property as required while making the ones that were
// optional nullable. Real codex is constrained by that schema server-side and
// therefore always emits every declared key; a canned scenario cannot know
// which keys a newly added optional property introduced, and used to fail with
// `missing required field` on that backend alone the moment the pipeline
// declared one. Nullability is the discriminator: only a property the original
// schema left optional is nullable, so a genuinely missing REQUIRED field
// still fails exactly as before. schemaPath == "" is a no-op.
func filterStructuredToSchema(structured map[string]any, schemaPath string) (map[string]any, error) {
	if schemaPath == "" {
		return structured, nil
	}
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("read schema %s: %w", schemaPath, err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("parse schema %s: %w", schemaPath, err)
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		return structured, nil
	}
	filtered := make(map[string]any, len(properties))
	for key, value := range structured {
		if _, ok := properties[key]; ok {
			filtered[key] = value
		}
	}
	for key, property := range properties {
		if _, ok := filtered[key]; ok {
			continue
		}
		if schemaAllowsNull(property) {
			filtered[key] = nil
		}
	}
	return filtered, nil
}

// schemaAllowsNull reports whether a property schema accepts null, which for a
// codex-normalized schema means the original schema left that property
// optional.
func schemaAllowsNull(property any) bool {
	schema, ok := property.(map[string]any)
	if !ok {
		return false
	}
	switch typ := schema["type"].(type) {
	case string:
		return typ == "null"
	case []any:
		for _, item := range typ {
			if name, ok := item.(string); ok && name == "null" {
				return true
			}
		}
	}
	return false
}

// extractCodexPrompt mirrors Codex's stdin prompt contract. Real codex argv is
// `codex exec [user-flags...] - --json [...]` for a fresh session and
// `codex exec resume [user-flags...] <session-id> - --json [...]` for a
// resumed session; in both cases the complete prompt is read to EOF from stdin.
func extractCodexPrompt(args []string, promptReader io.Reader) (string, error) {
	flagsWithValues := map[string]bool{
		"-m": true, "--model": true,
		"--sandbox": true, "--ask-for-approval": true,
		"--config": true, "--profile": true,
		"--output-schema":    true,
		"--reasoning-effort": true, "--reasoning-summary": true,
		"--color": true,
		"-c":      true, "--cd": true,
	}
	start := 0
	for i, a := range args {
		if a == "exec" {
			start = i + 1
			break
		}
	}
	var positionals []string
	for i := start; i < len(args); i++ {
		a := args[i]
		if flagsWithValues[a] {
			i++
			continue
		}
		if len(a) > 1 && a[0] == '-' {
			continue
		}
		positionals = append(positionals, a)
	}
	if len(positionals) == 0 {
		return "", fmt.Errorf("missing stdin prompt marker")
	}
	if positionals[0] == "resume" {
		if len(positionals) != 3 || positionals[2] != "-" {
			return "", fmt.Errorf("resume missing stdin prompt marker")
		}
	} else if len(positionals) != 1 || positionals[0] != "-" {
		return "", fmt.Errorf("missing stdin prompt marker")
	}
	prompt, err := io.ReadAll(promptReader)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(prompt), nil
}
