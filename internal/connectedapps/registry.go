// Package connectedapps lets Lumina act as a per-user MCP client to the user's
// other first-party apps. This file is the closed list of those apps: endpoints
// come from config, never from a user, and only read tools are allowlisted.
package connectedapps

import "lumina/internal/config"

// AuthMode is how Lumina authenticates to an app's MCP endpoint.
type AuthMode string

const (
	AuthOAuth AuthMode = "oauth"
	AuthPAT   AuthMode = "pat"
)

// App is one connectable first-party app.
type App struct {
	ID, Name, Blurb string // Blurb: one line on what Lumina can read
	MCPURL          string // Streamable HTTP endpoint
	Auth            AuthMode
	PATPrefix       string   // "nvq_pat_" for Norviq; "" otherwise
	ReadTools       []string // allowlist of the app's own tool names; none writes
}

// Registry is the ordered set of apps.
type Registry struct{ apps []App }

// NewRegistry builds the registry. ReadTools come from each app's own MCP
// catalog and hold only tools that never write.
func NewRegistry(cfg config.Config) Registry {
	return Registry{apps: []App{
		{
			ID: "norviq", Name: "Norviq",
			Blurb:     "Your portfolio, spending, budgets, goals and market watchlists",
			MCPURL:    cfg.ConnectedAppsNorviqMCPURL,
			Auth:      AuthPAT,
			PATPrefix: "nvq_pat_",
			// norviq-mcp/internal/tools/*.go: every tool annotated ReadOnlyHint.
			ReadTools: []string{
				"check_retirement_readiness", "export_expenses_csv", "get_dca_capacity",
				"get_insights", "get_market_brief", "get_news", "get_pilot",
				"get_pilot_follow", "get_portfolio_summary", "get_quote",
				"get_spending_report", "get_tax_dashboard", "get_tax_loss_carryforwards",
				"get_terminal_position", "get_terminal_positions", "list_budget_items",
				"list_budget_snapshots", "list_expense_categories", "list_expenses",
				"list_goals", "list_pilot_follows", "list_pilots", "list_positions",
				"list_price_targets", "list_recurring_expenses", "list_research_notes",
				"list_transactions", "list_watchlist", "list_watchlist_lists",
				"lookup_share_facts", "project_investment_growth", "search_symbols",
			},
		},
		{
			ID: "aaru", Name: "Aaru",
			Blurb:  "Your media library, what airs next and the public title catalog",
			MCPURL: cfg.ConnectedAppsAaruMCPURL,
			Auth:   AuthOAuth,
			// aaru-server-v2/internal/mcp/catalog.go: the tools with Writes unset.
			ReadTools: []string{"read_library", "list_airs", "search_titles"},
		},
		{
			ID: "khepri", Name: "Khepri",
			Blurb:  "Your goals, training, nutrition, check-ins and knowledge notes",
			MCPURL: cfg.ConnectedAppsKhepriMCPURL,
			Auth:   AuthOAuth,
			// north-client/internal/mcpserver/mcpserver.go (ReadOnlyHint tools) plus
			// the internal/agent capabilities with ReadOnly: true. ask_coach writes.
			ReadTools: []string{
				"search_goals", "list_check_ins", "search_knowledge", "knowledge_status",
				"get_fitness_summary", "search_exercises", "get_exercise", "list_goals",
				"search_ingredients", "todays_nutrition", "search_documents",
				"get_workout_plan", "get_meal_plan", "list_medications", "list_alerts",
				"get_stats", "get_lift_stats", "get_training_context", "get_training_week",
			},
		},
		{
			ID: "loci", Name: "Loci",
			Blurb:  "Your itineraries, saved lists, favorites and place search",
			MCPURL: cfg.ConnectedAppsLociMCPURL,
			// loci-connect-server/internal/mcp/middleware.go takes a loci_sk_ API key
			// as a Bearer token and serves no oauth-protected-resource document.
			Auth:      AuthPAT,
			PATPrefix: "loci_sk_",
			// loci-connect-server/internal/mcp/scopes.go readOnlyTools.
			ReadTools: []string{
				"status", "search_pois", "get_poi_details", "find_nearby",
				"list_itineraries", "get_itinerary", "list_user_lists", "get_list",
				"list_favorites",
			},
		},
	}}
}

// All returns the apps in display order.
func (r Registry) All() []App { return append([]App(nil), r.apps...) }

// Get returns the app with this id.
func (r Registry) Get(id string) (App, bool) {
	for _, a := range r.apps {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}
