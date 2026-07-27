package main

import (
	"fmt"

	"github.com/rysh-ai/rysh-cli-code/internal/progname"
)

// usageLine prints one line of usage text with the invoked binary name
// substituted for the literal "rysh". See package progname.
func usageLine(s string) {
	fmt.Println(progname.Rewrite(s))
}
