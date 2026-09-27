// Package prompts ships the setup prompt inside the binary.
package prompts

import _ "embed"

// Setup is prompts/setup.md, printed by `wassup init --print-prompt` and
// copied into .wassup/prompts/ on first run.
//
//go:embed setup.md
var Setup string
