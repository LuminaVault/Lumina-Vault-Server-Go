package main

import (
	"fmt"
	"strconv"
	"strings"
)

// Command is the parsed command line. The shape mirrors the Swift binary so the
// Helm chart (ENTRYPOINT ./App, CMD --http-host/--http-port, migrate job args
// ["migrate"]) runs either image unchanged.
type Command struct {
	Name     string
	HTTPHost string
	HTTPPort int
	Flags    map[string]string
}

var subcommands = map[string]bool{
	"serve":                        true,
	"migrate":                      true,
	"bootstrap-admin":              true,
	"backfill-hermes-profiles":     true,
	"backfill-vault-space-folders": true,
}

// parseArgs applies the Swift rule (Sources/App/App.swift): the first argument
// without a leading "-" is the subcommand, and the default is serve.
// --http-host and --http-port take a value; any other --flag is boolean unless
// written --flag=value.
func parseArgs(args []string) (Command, error) {
	c := Command{Name: "serve", HTTPHost: "127.0.0.1", HTTPPort: 8080, Flags: map[string]string{}}
	nameSet := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			if nameSet {
				return Command{}, fmt.Errorf("unexpected argument %q", a)
			}
			if !subcommands[a] {
				return Command{}, fmt.Errorf("unknown subcommand %q", a)
			}
			c.Name, nameSet = a, true
			continue
		}
		key, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch key {
		case "http-host", "http-port":
			if !hasValue {
				rest := args[i+1:]
				if len(rest) == 0 {
					return Command{}, fmt.Errorf("--%s needs a value", key)
				}
				value = rest[0]
				i++
			}
			if key == "http-host" {
				c.HTTPHost = value
				continue
			}
			port, err := strconv.Atoi(value)
			if err != nil || port < 1 || port > 65535 {
				return Command{}, fmt.Errorf("invalid --http-port %q", value)
			}
			c.HTTPPort = port
		default:
			if !hasValue {
				value = "true"
			}
			c.Flags[key] = value
		}
	}
	return c, nil
}
