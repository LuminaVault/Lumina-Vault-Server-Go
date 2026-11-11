package me

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Ports of BriefComposerTests' pure cases.

func TestBriefSectionsSplitOnLabels(t *testing.T) {
	md := "Good morning!\n\n## Weather\nSunny, 21°.\n\n**Inbox:** two from Ana\n**On your mind**\n- the trip\n\n## Empty\n\n**Misc** — leftovers"
	got := briefSections(md)
	require.Equal(t, []briefSection{
		{Weather, "Weather", "Sunny, 21°."},
		{Inbox, "Inbox", "two from Ana"},
		{OnYourMind, "On your mind", "- the trip"},
		{Other, "Misc", "leftovers"},
	}, got)
}

func TestBriefSectionsWithoutLabelsIsOneCard(t *testing.T) {
	require.Equal(t, []briefSection{{OnYourMind, "On your mind", "just text"}}, briefSections("  just text \n"))
	require.Nil(t, briefSections(" \n "))
}

func TestBriefLabelEmphasisIsNotALabel(t *testing.T) {
	_, _, ok := briefLabel("**Really** important thing")
	require.False(t, ok)
	title, rest, ok := briefLabel("**Weather:** sunny")
	require.True(t, ok)
	require.Equal(t, "Weather", title)
	require.Equal(t, "sunny", rest)
	_, _, ok = briefLabel("## **  **")
	require.False(t, ok)
	_, _, ok = briefLabel("**" + string(make([]byte, 0)) + "a very long bold phrase that keeps going on and on**")
	require.False(t, ok)
}

func TestKindForHeading(t *testing.T) {
	require.Equal(t, Today, kindForHeading("Today"))
	require.Equal(t, Today, kindForHeading("Your schedule"))
	require.Equal(t, Other, kindForHeading("Today's news"))
	require.Equal(t, Inbox, kindForHeading("Email"))
}

func TestUniqueIDsAndOrder(t *testing.T) {
	cards := uniqueCardIDs([]BriefCardDTO{{Kind: Other, Id: "other"}, {Kind: Weather, Id: "weather"}, {Kind: Other, Id: "other"}})
	require.Equal(t, "other-2", cards[2].Id)
	ordered := orderCards(cards, map[string]int{"other": -2, "weather": -1})
	require.Equal(t, []string{"weather", "other", "other-2"}, []string{ordered[0].Id, ordered[1].Id, ordered[2].Id})
}

func TestOpenLoopsCard(t *testing.T) {
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	due := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	today := time.Date(2026, 3, 10, 18, 0, 0, 0, time.UTC)
	card := openLoopsCard([]commitmentRow{
		{ID: uuid.MustParse("00000000-0000-4000-8000-0000000000aa"), Text: "Call Bob", DueAt: &due},
		{ID: uuid.New(), Text: "Pay rent", DueAt: &today},
		{ID: uuid.New(), Text: "Someday"},
	}, time.UTC, now)
	require.Equal(t, "- Call Bob (due Mon 9 Mar)\n- Pay rent (due Tue 10 Mar)\n- Someday", card.Body)
	require.Equal(t, "You said you'd do these — one is overdue", *card.Why)
	require.Equal(t, "00000000-0000-4000-8000-0000000000AA", card.Refs[0].Id)
	require.Nil(t, openLoopsCard(nil, time.UTC, now))
}

func TestLearnedPushBody(t *testing.T) {
	_, ok := learnedPushBody(0, 0)
	require.False(t, ok)
	b, _ := learnedPushBody(1, 0)
	require.Equal(t, "1 thing about you — keep, edit or forget it.", b)
	b, _ = learnedPushBody(2, 1)
	require.Equal(t, "2 things about you and 1 decision — keep, edit or forget them.", b)
}
