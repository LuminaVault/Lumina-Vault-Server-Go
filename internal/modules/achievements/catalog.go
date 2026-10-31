package achievements

// Port of Sources/App/Achievements/AchievementCatalog.swift. The catalog is
// static and must stay byte-for-byte equal to Swift's: order, keys, labels,
// events and targets. Bump CatalogVersion only together with Swift.

// Event is Swift's AchievementEvent: a usage event that moves counters.
type Event string

const (
	EventMemoryUpserted Event = "memory_upserted"
	EventChatCompleted  Event = "chat_completed"
	EventKBCompiled     Event = "kb_compiled"
	EventQueryRan       Event = "query_ran"
	EventVaultUploaded  Event = "vault_uploaded"
	EventSoulConfigured Event = "soul_configured"
	EventSpaceCreated   Event = "space_created"
)

// AllEvents is AchievementEvent.allCases, in declaration order.
var AllEvents = []Event{
	EventMemoryUpserted, EventChatCompleted, EventKBCompiled, EventQueryRan,
	EventVaultUploaded, EventSoulConfigured, EventSpaceCreated,
}

// Sub is Swift's SubAchievement. Key is `<archetype>.<slug>`, the row key in
// achievement_progress; Target is the inclusive unlock threshold.
type Sub struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Event  Event  `json:"event"`
	Target int64  `json:"target"`
}

// Archetype is Swift's AchievementArchetype.
type Archetype struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Subs  []Sub  `json:"subs"`
}

// Catalog is Swift's AchievementCatalog.
type Catalog struct {
	CatalogVersion int
	Archetypes     []Archetype
}

// Current is AchievementCatalog.current.
var Current = Catalog{
	CatalogVersion: 1,
	Archetypes: []Archetype{
		{Key: "lightbringer", Label: "Lightbringer", Subs: []Sub{
			{Key: "lightbringer.first-spark", Label: "First Spark", Event: EventMemoryUpserted, Target: 1},
			{Key: "lightbringer.kindled-mind", Label: "Kindled Mind", Event: EventMemoryUpserted, Target: 10},
			{Key: "lightbringer.illuminator", Label: "Illuminator", Event: EventMemoryUpserted, Target: 50},
			{Key: "lightbringer.lightbearer", Label: "Lightbearer", Event: EventMemoryUpserted, Target: 200},
		}},
		{Key: "shadowlord", Label: "Shadowlord", Subs: []Sub{
			{Key: "shadowlord.shadow-touched", Label: "Shadow-touched", Event: EventSoulConfigured, Target: 1},
			{Key: "shadowlord.deep-listener", Label: "Deep Listener", Event: EventChatCompleted, Target: 5},
			{Key: "shadowlord.night-walker", Label: "Night Walker", Event: EventChatCompleted, Target: 25},
			{Key: "shadowlord.umbral-sovereign", Label: "Umbral Sovereign", Event: EventChatCompleted, Target: 100},
		}},
		{Key: "reignmaker", Label: "Reignmaker", Subs: []Sub{
			{Key: "reignmaker.first-edict", Label: "First Edict", Event: EventQueryRan, Target: 1},
			{Key: "reignmaker.tactician", Label: "Tactician", Event: EventQueryRan, Target: 10},
			{Key: "reignmaker.strategist", Label: "Strategist", Event: EventKBCompiled, Target: 5},
			{Key: "reignmaker.regent", Label: "Regent", Event: EventQueryRan, Target: 100},
		}},
		{Key: "soulseeker", Label: "Soulseeker", Subs: []Sub{
			{Key: "soulseeker.first-relic", Label: "First Relic", Event: EventVaultUploaded, Target: 1},
			{Key: "soulseeker.collector", Label: "Collector", Event: EventVaultUploaded, Target: 10},
			{Key: "soulseeker.cartographer", Label: "Cartographer", Event: EventSpaceCreated, Target: 3},
			{Key: "soulseeker.soulkeeper", Label: "Soulkeeper", Event: EventVaultUploaded, Target: 100},
		}},
	},
}

// SubsMatching is AchievementCatalog.subs(matching:): order-preserving over
// the archetypes. Unused by the read routes; kept for the Go producers that
// will record progress once the AchievementsWorker moves.
func (c Catalog) SubsMatching(e Event) []Sub {
	var out []Sub
	for _, a := range c.Archetypes {
		for _, s := range a.Subs {
			if s.Event == e {
				out = append(out, s)
			}
		}
	}
	return out
}

// SubsByKey is AchievementCatalog.subsByKey.
func (c Catalog) SubsByKey() map[string]Sub {
	out := map[string]Sub{}
	for _, a := range c.Archetypes {
		for _, s := range a.Subs {
			out[s.Key] = s
		}
	}
	return out
}
