package main

import (
	"fmt"
	"os"
)

const version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}

	fmt.Fprintln(os.Stderr, "zenchron-engineering: Engineering Authorization Kernel bootstrap")
	fmt.Fprintln(os.Stderr, "usage: zenchron-engineering version")
}
