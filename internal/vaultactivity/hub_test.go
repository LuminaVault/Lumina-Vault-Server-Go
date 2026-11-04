package vaultactivity_test

import (
	"lumina/internal/vaultactivity"
	"lumina/internal/wire"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestHubDeliversByVaultAndUnsubscribes(t *testing.T) {
	h := vaultactivity.NewHub()
	a, b := uuid.New(), uuid.New()
	chA, cancelA := h.Subscribe(a)
	chA2, cancelA2 := h.Subscribe(a)
	chB, cancelB := h.Subscribe(b)
	defer cancelB()

	ev := vaultactivity.Activity{ID: wire.UUID{UUID: uuid.New()}, VaultID: wire.UUID{UUID: a}, Action: "x"}
	h.Publish(ev)
	require.Equal(t, ev, <-chA)
	require.Equal(t, ev, <-chA2)
	require.Empty(t, chB, "other vaults hear nothing")

	cancelA()
	cancelA() // idempotent
	require.Equal(t, 1, h.Subscribers(a))
	cancelA2()
	require.Equal(t, 0, h.Subscribers(a))
	h.Publish(ev) // no subscribers: no panic, nothing queued
	require.Empty(t, chA)
}

func TestHubDropsWhenASubscriberIsFull(t *testing.T) {
	h := vaultactivity.NewHub()
	v := uuid.New()
	ch, cancel := h.Subscribe(v)
	defer cancel()
	for range 300 {
		h.Publish(vaultactivity.Activity{VaultID: wire.UUID{UUID: v}})
	}
	require.Len(t, ch, cap(ch), "publish never blocks")
}
