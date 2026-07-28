package main

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"strings"

	"hfsgo/internal/gui"
)

//go:embed logo.png
var logoPNG []byte

//go:embed dashang.png
var donationPNG []byte

func main() {
	runClient := strings.Contains(strings.ToLower(filepath.Base(os.Args[0])), "client")
	for _, argument := range os.Args[1:] {
		if strings.EqualFold(strings.TrimSpace(argument), "--client") {
			runClient = true
			break
		}
	}
	var err error
	if runClient {
		err = gui.RunClient(logoPNG)
	} else {
		err = gui.Run(logoPNG, donationPNG)
	}
	if err != nil {
		log.Fatal(err)
	}
}
