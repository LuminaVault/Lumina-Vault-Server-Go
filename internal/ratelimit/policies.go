package ratelimit

import "time"

// Wave 5's policies, with RateLimitMiddleware.swift's numbers. All but
// MCPAnonymousByIP are keyed by UserOrIPKey. Scale them with TieredMiddleware only
// where TierScale is set: the others have the same ceiling for every tier.
var (
	// ChatByUser is shared by every /v1/llm route.
	ChatByUser = Policy{Name: "chatByUser", Max: 30, Window: 60 * time.Second, TierScale: StandardTierScale}
	// ConversationByUser also covers /v1/hermes/runs in Swift.
	ConversationByUser = Policy{Name: "conversationByUser", Max: 30, Window: 60 * time.Second, TierScale: StandardTierScale}
	// SkillRunByUser is manual runs only. Cron and event runs bypass the middleware.
	SkillRunByUser = Policy{Name: "skillRunByUser", Max: 10, Window: 60 * time.Second, TierScale: StandardTierScale}
	// SkillOutputsByUser is the Today feed, a polled read, so it has no scale.
	SkillOutputsByUser = Policy{Name: "skillOutputsByUser", Max: 60, Window: 60 * time.Second}
	JobsByUser         = Policy{Name: "jobsByUser", Max: 20, Window: 60 * time.Second, TierScale: StandardTierScale}
	WorkflowsByUser    = Policy{Name: "workflowsByUser", Max: 60, Window: 60 * time.Second, TierScale: StandardTierScale}

	// MCPAnonymousByIP is keyed by IPOnlyKey: unauthenticated /v1/mcp attempts.
	MCPAnonymousByIP = Policy{Name: "mcpAnonymousByIP", Max: 600, Window: 60 * time.Second}
	MCPByUser        = Policy{Name: "mcpByUser", Max: 120, Window: 60 * time.Second}
)

// Stacked voice and speech policies: a route applies both of its pair. Only the two
// /v1/audio daily caps scale. Scaling a per-minute bucket would give a free user one
// voice note a minute, and burst shape isn't what varies with what someone pays.
var (
	TranscribeByUserPerMinute = Policy{Name: "transcribeByUserPerMinute", Max: 10, Window: 60 * time.Second}
	TranscribeByUserDaily     = Policy{Name: "transcribeByUserDaily", Max: 200, Window: 86400 * time.Second}

	AudioTranscriptionsByUserPerMinute = Policy{Name: "audioTranscriptionsByUserPerMinute", Max: 20, Window: 60 * time.Second}
	AudioTranscriptionsByUserDaily     = Policy{Name: "audioTranscriptionsByUserDaily", Max: 200, Window: 86400 * time.Second, TierScale: VoiceTierScale}

	AudioSpeechByUserPerMinute = Policy{Name: "audioSpeechByUserPerMinute", Max: 30, Window: 60 * time.Second}
	AudioSpeechByUserDaily     = Policy{Name: "audioSpeechByUserDaily", Max: 1000, Window: 86400 * time.Second, TierScale: VoiceTierScale}

	TTSByUserPerMinute = Policy{Name: "ttsByUserPerMinute", Max: 30, Window: 60 * time.Second}
	TTSByUserDaily     = Policy{Name: "ttsByUserDaily", Max: 1000, Window: 86400 * time.Second}
)
