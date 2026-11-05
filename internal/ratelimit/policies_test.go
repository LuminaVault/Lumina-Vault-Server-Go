package ratelimit_test

import (
	"lumina/internal/ratelimit"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var standard = reflect.ValueOf(ratelimit.StandardTierScale).Pointer()

var voice = reflect.ValueOf(ratelimit.VoiceTierScale).Pointer()

// Every number here was read off RateLimitMiddleware.swift at fdbed9b0, one
// definition at a time, including which have a scale.
func TestPolicyNumbersMatchSwift(t *testing.T) {
	const (
		minute = 60 * time.Second
		day    = 86400 * time.Second
	)
	cases := []struct {
		policy ratelimit.Policy
		name   string
		max    int
		window time.Duration
		scale  uintptr
	}{
		{ratelimit.ChatByUser, "chatByUser", 30, minute, standard},
		{ratelimit.ConversationByUser, "conversationByUser", 30, minute, standard},
		{ratelimit.SkillRunByUser, "skillRunByUser", 10, minute, standard},
		{ratelimit.SkillOutputsByUser, "skillOutputsByUser", 60, minute, 0},
		{ratelimit.JobsByUser, "jobsByUser", 20, minute, standard},
		{ratelimit.WorkflowsByUser, "workflowsByUser", 60, minute, standard},
		{ratelimit.MCPAnonymousByIP, "mcpAnonymousByIP", 600, minute, 0},
		{ratelimit.MCPByUser, "mcpByUser", 120, minute, 0},
		{ratelimit.TranscribeByUserPerMinute, "transcribeByUserPerMinute", 10, minute, 0},
		{ratelimit.TranscribeByUserDaily, "transcribeByUserDaily", 200, day, 0},
		{ratelimit.AudioTranscriptionsByUserPerMinute, "audioTranscriptionsByUserPerMinute", 20, minute, 0},
		{ratelimit.AudioTranscriptionsByUserDaily, "audioTranscriptionsByUserDaily", 200, day, voice},
		{ratelimit.AudioSpeechByUserPerMinute, "audioSpeechByUserPerMinute", 30, minute, 0},
		{ratelimit.AudioSpeechByUserDaily, "audioSpeechByUserDaily", 1000, day, voice},
		{ratelimit.TTSByUserPerMinute, "ttsByUserPerMinute", 30, minute, 0},
		{ratelimit.TTSByUserDaily, "ttsByUserDaily", 1000, day, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.name, tc.policy.Name)
			require.Equal(t, tc.max, tc.policy.Max)
			require.Equal(t, tc.window, tc.policy.Window)
			if tc.scale == 0 {
				require.Nil(t, tc.policy.TierScale, "a flat policy")
				require.Equal(t, tc.max, tc.policy.EffectiveMax("free"))
				require.Equal(t, tc.max, tc.policy.EffectiveMax("ultimate"))
				return
			}
			require.Equal(t, tc.scale, reflect.ValueOf(tc.policy.TierScale).Pointer())
		})
	}
}

// A duplicate name silently merges two buckets, which is what the namespace exists to prevent.
func TestEveryPolicyNameIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range []ratelimit.Policy{
		ratelimit.SettingsByUser, ratelimit.AchievementsByUser, ratelimit.CaptureByUser, ratelimit.MeTodayByUser,
		ratelimit.VaultUploadByUser, ratelimit.VaultExportByUser, ratelimit.QueryByUser,
		ratelimit.VisionEmbedByUserPerMinute, ratelimit.VisionEmbedByUserDaily,
		ratelimit.ChatByUser, ratelimit.ConversationByUser, ratelimit.SkillRunByUser, ratelimit.SkillOutputsByUser,
		ratelimit.JobsByUser, ratelimit.WorkflowsByUser, ratelimit.MCPAnonymousByIP, ratelimit.MCPByUser,
		ratelimit.TranscribeByUserPerMinute, ratelimit.TranscribeByUserDaily,
		ratelimit.AudioTranscriptionsByUserPerMinute, ratelimit.AudioTranscriptionsByUserDaily,
		ratelimit.AudioSpeechByUserPerMinute, ratelimit.AudioSpeechByUserDaily,
		ratelimit.TTSByUserPerMinute, ratelimit.TTSByUserDaily,
	} {
		require.NotEmpty(t, p.Name)
		require.False(t, seen[p.Name], p.Name)
		seen[p.Name] = true
	}
}

// "query policy uses the per user keyer": same budget as chat, because /v1/query
// costs what chat costs.
func TestQueryBudgetMatchesChat(t *testing.T) {
	require.Equal(t, ratelimit.ChatByUser.Max, ratelimit.QueryByUser.Max)
	require.Equal(t, ratelimit.ChatByUser.Window, ratelimit.QueryByUser.Window)
}

// TierAwareRateLimitTests.
func TestPaidTiersGetHeadroomAboveTheBaseline(t *testing.T) {
	require.Equal(t, 30, ratelimit.ChatByUser.EffectiveMax("trial"))
	require.Equal(t, 60, ratelimit.ChatByUser.EffectiveMax("pro"))
	require.Equal(t, 120, ratelimit.ChatByUser.EffectiveMax("ultimate"))
}

func TestFreeAndLapsedShareASmallWorkingBucket(t *testing.T) {
	require.Equal(t, 10, ratelimit.ChatByUser.EffectiveMax("lapsed"))
	require.Equal(t, 10, ratelimit.ChatByUser.EffectiveMax("free"))
}

func TestNoTierIsLockedOutByAScale(t *testing.T) {
	for _, scale := range []func(string) float64{ratelimit.StandardTierScale, ratelimit.VoiceTierScale} {
		tight := ratelimit.Policy{Name: "tight", Max: 1, Window: time.Minute, TierScale: scale}
		for _, tier := range []string{"free", "trial", "pro", "ultimate", "lapsed", "archived", "unknown"} {
			require.GreaterOrEqual(t, tight.EffectiveMax(tier), 1, tier)
		}
	}
}

func TestVoiceLadder(t *testing.T) {
	cases := []struct {
		tier         string
		transcribe   int
		speech       int
		minuteBucket int
	}{
		{"ultimate", 400, 2000, 20},
		{"pro", 300, 1500, 20},
		{"trial", 200, 1000, 20},
		{"free", 10, 50, 20}, // "0.05 of the 200/day daily cap is 10 voice messages a day"
		{"lapsed", 10, 50, 20},
		{"archived", 10, 50, 20},
		{"not-a-tier", 10, 50, 20}, // tierEnum reads as lapsed
	}
	for _, tc := range cases {
		require.Equal(t, tc.transcribe, ratelimit.AudioTranscriptionsByUserDaily.EffectiveMax(tc.tier), tc.tier)
		require.Equal(t, tc.speech, ratelimit.AudioSpeechByUserDaily.EffectiveMax(tc.tier), tc.tier)
		require.Equal(t, tc.minuteBucket, ratelimit.AudioTranscriptionsByUserPerMinute.EffectiveMax(tc.tier),
			"the per-minute bucket isn't scaled, or a free user gets one voice note a minute")
	}
}
