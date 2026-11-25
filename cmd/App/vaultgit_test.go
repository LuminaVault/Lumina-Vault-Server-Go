package main

import (
	"lumina/internal/config"
	"lumina/internal/vaultpaths"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVaultGitIsOffByDefault(t *testing.T) {
	_, registered, stop := vaultGit(nil, config.Config{VaultGitEnabled: "false"}, vaultpaths.Paths{Root: t.TempDir()}, nil)
	defer stop()
	require.Empty(t, registered)
}

func TestVaultGitRegistersSweepAndGC(t *testing.T) {
	_, registered, stop := vaultGit(nil, config.Config{VaultGitEnabled: "true"}, vaultpaths.Paths{Root: t.TempDir()}, nil)
	defer stop()
	names := []string{}
	for _, j := range registered {
		names = append(names, j.Name)
	}
	require.Equal(t, []string{"VaultGitSweep", "VaultGitGC"}, names)
}
