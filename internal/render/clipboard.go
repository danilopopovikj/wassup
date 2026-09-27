package render

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// copyCmd copies text with OSC 52 (works inside tmux with set-clipboard on)
// and, best effort, with the platform clipboard tool.
func copyCmd(text string) tea.Cmd {
	go systemCopy(text)
	return tea.SetClipboard(text)
}

func systemCopy(text string) {
	var candidates [][]string
	switch runtime.GOOS {
	case "darwin":
		candidates = [][]string{{"pbcopy"}}
	default:
		candidates = [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}}
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return
		}
	}
}

// CopyText copies outside a running program (CLI use): the platform tool
// first, then a bare OSC 52 sequence on stdout when it looks like a terminal.
func CopyText(text string) {
	systemCopy(text)
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprintf(os.Stdout, "\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte(text)))
	}
}
