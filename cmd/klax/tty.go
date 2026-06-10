package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/PiDmitrius/klax/internal/claudetty/driver"
)

func runTTY(args []string) {
	opts, err := parseTTYArgs(args)
	if err != nil {
		log.Fatalf("tty: %v", err)
	}
	if opts.Prompt == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatalf("tty: read stdin: %v", err)
		}
		opts.Prompt = trimTrailingNewlines(string(b))
	}
	if opts.Prompt == "" {
		log.Fatal("tty: empty prompt (positional arg or stdin required)")
	}
	// /abort cancels the runner ctx, which SIGTERMs this wrapper's process
	// group. Turn that into a ctx cancel so driver.Run returns through its
	// defers (which reap claude's separate Setsid group and the temp dir)
	// instead of Go's default SIGTERM kill, which would skip every defer.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	code, err := driver.Run(ctx, os.Stdout, opts)
	if err != nil {
		log.Printf("tty: %v", err)
	}
	os.Exit(code)
}

func parseTTYArgs(args []string) (driver.Options, error) {
	var opts driver.Options
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return opts, fmt.Errorf("usage: klax tty [--] claude [claude -p flags] [prompt]")
	}
	opts.ClaudePath = args[0]
	args = args[1:]

	needValue := func(i int, flag string) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		var err error
		switch a {
		case "-p", "--print", "--verbose", "--include-partial-messages":
		case "--output-format":
			var v string
			if v, err = needValue(i, a); err == nil && v != "stream-json" {
				err = fmt.Errorf("unsupported output format %q (only stream-json)", v)
			}
			i++
		case "--model":
			opts.Model, err = needValue(i, a)
			i++
		case "--effort":
			opts.Effort, err = needValue(i, a)
			i++
		case "--permission-mode":
			opts.PermissionMode, err = needValue(i, a)
			i++
		case "--append-system-prompt":
			opts.AppendSystemPrompt, err = needValue(i, a)
			i++
		case "--disallowed-tools", "--disallowedTools":
			opts.DisallowedTools, err = needValue(i, a)
			i++
		case "--resume":
			opts.Resume, err = needValue(i, a)
			i++
		case "--timeout":
			var v string
			if v, err = needValue(i, a); err == nil {
				opts.Timeout, err = time.ParseDuration(v)
			}
			i++
		case "--debug":
			opts.Debug = os.Stderr
		default:
			if len(a) > 0 && a[0] == '-' {
				return opts, fmt.Errorf("unsupported flag %q", a)
			}
			if opts.Prompt != "" {
				return opts, fmt.Errorf("multiple positional prompts")
			}
			opts.Prompt = a
		}
		if err != nil {
			return opts, err
		}
	}
	return opts, nil
}

func trimTrailingNewlines(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
