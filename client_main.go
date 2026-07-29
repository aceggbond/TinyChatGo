//go:build client

package main

import (
	_ "embed"
	"log"

	"lanchatgo/internal/gui"
)

//go:embed logo.png
var clientLogoPNG []byte

func main() {
	if err := gui.RunClient(clientLogoPNG); err != nil {
		log.Fatal(err)
	}
}
