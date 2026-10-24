package main

import (
	"flag"
	"fmt"
	"os"

	"bigo-live-recorder/internal/app"
	"bigo-live-recorder/internal/state"
)

func main() {
	configDir := flag.String("config-dir", "", "override the configuration directory")
	preview := flag.Bool("preview", false, "show the design with sample data; no monitoring or recording")
	flag.Parse()
	var err error
	if *configDir == "" {
		*configDir, err = state.DefaultDirectory()
	}
	if err == nil {
		err = app.Run(*configDir, *preview)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
