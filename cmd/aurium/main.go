// Command aurium is the Aurium CLI.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/RhyChaw/aurium/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		// Exit codes are part of the interface (§12.1) so scripts and the
		// daemon can branch on the failure kind rather than parsing text.
		var coded *cli.ExitError
		if errors.As(err, &coded) {
			if coded.Message != "" {
				fmt.Fprintln(os.Stderr, "aurium: "+coded.Message)
			}
			os.Exit(coded.Code)
		}
		fmt.Fprintln(os.Stderr, "aurium: "+err.Error())
		os.Exit(1)
	}
}
