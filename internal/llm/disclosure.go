package llm

import (
	"context"

	"github.com/google/uuid"
)

// Disclosure is ModelDisclosure: whether the concrete upstream model identity may
// reach the client. Managed tenants never learn which model served a turn; BYOK
// tenants chose their own and keep full visibility. Server-side telemetry always
// keeps the real model: this governs the client wire surface only.
type Disclosure int

const (
	// DisclosureHidden is the default: anything not explicitly BYOK.
	DisclosureHidden Disclosure = iota
	DisclosureVisible
)

// DisclosureForMode is ModelDisclosure.forBrainMode: visible only for BYOK. Pass
// "" for a tenant with no preference row.
func DisclosureForMode(mode CredentialMode) Disclosure {
	if mode == ModeBYOK {
		return DisclosureVisible
	}
	return DisclosureHidden
}

// DisclosureFor resolves a tenant's disclosure from its preference row, as
// RoutedHermesLLMService.disclosure(sessionKey:) does: hidden unless the session
// key parses as a tenant UUID whose row says BYOK. No source, an unparsable key,
// a read error or no row are all hidden.
func DisclosureFor(ctx context.Context, prefs PreferenceSource, sessionKey string) Disclosure {
	if prefs == nil {
		return DisclosureHidden
	}
	tenant, err := uuid.Parse(sessionKey)
	if err != nil {
		return DisclosureHidden
	}
	pref, err := prefs.Get(ctx, tenant)
	if err != nil || pref == nil {
		return DisclosureHidden
	}
	return DisclosureForMode(pref.Mode)
}

const (
	// GenericModelID is ModelDisclosurePolicy.genericModelID, sent in place of a
	// real upstream model id.
	GenericModelID = "auto"
	// GenericBrainName is the dashboard hero label for managed tenants.
	GenericBrainName = "LuminaVault Brain · Auto"
	// GenericProviderName is the dashboard provider label for managed tenants.
	GenericProviderName = "LuminaVault"
	// GenericFallbackMessage is the user message on a scrubbed fallback notice.
	GenericFallbackMessage = "Your brain switched to a backup route to finish this reply."
)

// SystemPromptGuard is ModelDisclosurePolicy.systemPromptGuard. It is the same
// string as ModelIdentityGuard, which 4F named first.
const SystemPromptGuard = ModelIdentityGuard

// GenericLabel is ModelDisclosurePolicy.genericLabel(task:), "Auto · coding".
func GenericLabel(task string) string { return "Auto · " + task }

// ScrubCerberusEvent is ModelDisclosurePolicy.scrub for the routing and usage
// events the routed services publish on Values.CerberusSink. Visible passes
// through. Hidden empties the routing event's routes, sets DisplayLabel, keeps
// PromptTokens and drops ContextWindowTokens (a window size fingerprints the
// model); on usage it drops the provider and model.
func ScrubCerberusEvent(event CerberusEvent, d Disclosure) CerberusEvent {
	if d == DisclosureVisible {
		return event
	}
	out := event
	if r := event.Routing; r != nil {
		scrubbed := *r
		scrubbed.ActiveRoutes = []WireRoute{}
		label := GenericLabel(r.TaskType)
		scrubbed.DisplayLabel = &label
		scrubbed.ContextWindowTokens = nil
		out.Routing = &scrubbed
	}
	if u := event.Usage; u != nil {
		scrubbed := *u
		scrubbed.Provider = ""
		scrubbed.Model = ""
		out.Usage = &scrubbed
	}
	return out
}

// ScrubFailoverNotice is ModelDisclosurePolicy.scrub for a .fallback frame:
// hidden rewrites both routes to openRouter/auto and the message to
// GenericFallbackMessage, keeping only the reason code. The wire DTO has no
// status code, body or source, so those are cleared too.
func ScrubFailoverNotice(n FailoverNotice, d Disclosure) FailoverNotice {
	if d == DisclosureVisible {
		return n
	}
	return FailoverNotice{
		OriginalProvider: OpenRouter, OriginalModel: GenericModelID,
		FallbackProvider: OpenRouter, FallbackModel: GenericModelID,
		ReasonCode: n.ReasonCode, UserMessage: GenericFallbackMessage,
	}
}
