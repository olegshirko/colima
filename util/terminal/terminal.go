package terminal

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

var isTerminal = term.IsTerminal(int(os.Stdout.Fd()))

// ClearLine clears the previous line of the terminal
func ClearLine() {
	if !isTerminal {
		return
	}

	fmt.Print("\033[1A \033[2K \r")
}

// Progress returns a string of the progress
func Progress(current, total int64) string {
	if total <= 0 {
		return ""
	}
	return fmt.Sprintf("%.2f%%", float64(current)*100/float64(total))
}
