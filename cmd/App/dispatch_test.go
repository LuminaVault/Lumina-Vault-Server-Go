package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseArgs(t *testing.T) {
	c, err := parseArgs([]string{"--http-host", "0.0.0.0", "--http-port", "8080"})
	require.NoError(t, err)
	require.Equal(t, Command{Name: "serve", HTTPHost: "0.0.0.0", HTTPPort: 8080, Flags: map[string]string{}}, c)

	c, err = parseArgs([]string{"migrate"})
	require.NoError(t, err)
	require.Equal(t, "migrate", c.Name)

	c, err = parseArgs([]string{"migrate", "--adopt-fluent"})
	require.NoError(t, err)
	require.Equal(t, "true", c.Flags["adopt-fluent"])

	_, err = parseArgs([]string{"frobnicate"})
	require.ErrorContains(t, err, "unknown subcommand")
}

func TestParseArgsDefaults(t *testing.T) {
	c, err := parseArgs(nil)
	require.NoError(t, err)
	require.Equal(t, Command{Name: "serve", HTTPHost: "127.0.0.1", HTTPPort: 8080, Flags: map[string]string{}}, c)
}

func TestParseArgsEqualsForm(t *testing.T) {
	c, err := parseArgs([]string{"--http-port=9090"})
	require.NoError(t, err)
	require.Equal(t, 9090, c.HTTPPort)
}

func TestParseArgsRejectsBadPort(t *testing.T) {
	_, err := parseArgs([]string{"--http-port", "eighty"})
	require.ErrorContains(t, err, "http-port")
}
