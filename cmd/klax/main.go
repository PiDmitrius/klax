// klax daemon — AI messaging bridge.
// Uses claude -p --output-format stream-json for streaming responses.
package main

import (
	"fmt"
	"log"
	"os"
)

const version = "0.5.86"

func main() {
	log.SetPrefix("klax: ")

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "start":
		fg := len(os.Args) > 2 && os.Args[2] == "--foreground"
		if fg {
			runDaemon()
		} else {
			runServiceStart()
		}
	case "stop":
		runServiceCtl("stop")
	case "restart":
		runServiceCtl("restart")
	case "status":
		runStatus()
	case "install":
		runInstall()
	case "uninstall":
		runUninstall()
	case "update":
		runUpdate()
	case "fallback":
		runFallback()
	case "setup":
		runSetup()
	case "version":
		fmt.Printf("klax %s\n", version)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "klax %s — AI messaging bridge\n\n", version)
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  setup       Interactive first-time setup")
	fmt.Fprintln(os.Stderr, "  install     Install systemd user service")
	fmt.Fprintln(os.Stderr, "  uninstall   Remove systemd user service")
	fmt.Fprintln(os.Stderr, "  start       Start the service (--foreground to run directly)")
	fmt.Fprintln(os.Stderr, "  stop        Stop the service")
	fmt.Fprintln(os.Stderr, "  restart     Restart the service")
	fmt.Fprintln(os.Stderr, "  update      Download latest release and restart")
	fmt.Fprintln(os.Stderr, "  status      Show service status")
	fmt.Fprintln(os.Stderr, "  fallback    Install latest release from GitHub")
	fmt.Fprintln(os.Stderr, "  version     Print version")
}
