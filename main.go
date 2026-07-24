package main

import (
	_ "embed"
	"log"

	"hfsgo/internal/gui"
)

//go:embed logo.png
var logoPNG []byte

//go:embed dashang.png
var donationPNG []byte

func main() {
	if err := gui.Run(logoPNG, donationPNG); err != nil {
		log.Fatal(err)
	}
}
