// Package skill ships the Claude Code skill inside the binary so
// `wassup skill install` can copy it into a project.
package skill

import "embed"

// FS holds skill/wassup/** (SKILL.md and the reference files).
//
//go:embed all:wassup
var FS embed.FS
