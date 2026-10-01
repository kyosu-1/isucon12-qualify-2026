package main

import (
	"fmt"
	"os"

	isuports "github.com/isucon/isucon12-qualify/webapp/go"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := isuports.Migrate(); err != nil {
			fmt.Fprintln(os.Stderr, "migrate failed:", err)
			os.Exit(1)
		}
		return
	}
	isuports.Run()
}
