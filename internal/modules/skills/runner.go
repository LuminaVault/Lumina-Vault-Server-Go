package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/devicetools"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/modules/calendar"
	"lumina/internal/skills"
	"lumina/internal/swiftstr"
	"lumina/internal/usagemeter"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Runner is SkillRunner: a skill's body run through the routed transport with
// its manifest's allowed-tools enforced server-side, then its outputs dispatched
// and the run logged to skill_run_log and skills_state.
type Runner struct {
	pool       *pgxpool.Pool
	transport  llm.ChatTransport
	model      string
	paths      vaultpaths.Paths
	memories   *memorystore.Repository
	embeddings embedding.Service
	push       *apns.Service
	meter      *usagemeter.Meter
	proactive  *apns.ProactiveDelivery
	device     *devicetools.Tools
	google     *calendar.Google
	brief      BriefComposer
	http       *http.Client
	guard      capGuard
	log        *slog.Logger
	now        func() time.Time
}

// BriefComposer is the me module's ComposeBrief: today's brief as cards, the
// number of cards it holds.
type BriefComposer interface {
	ComposeBrief(ctx context.Context, tenant uuid.UUID, skillMarkdown string, now time.Time) (int, error)
}

// briefSkillName is BriefComposer.skillName.
const briefSkillName = "daily-brief"

// RunnerOptions are the runner's dependencies. Nil ones behave as Swift's do
// when absent: no meter skips the budget, no proactive delivery fails a
// chat_message output, no Google answers mail_inbox_recent with an error.
type RunnerOptions struct {
	Transport    llm.ChatTransport
	DefaultModel string
	Paths        vaultpaths.Paths
	Memories     *memorystore.Repository
	Embeddings   embedding.Service
	APNS         *apns.Service
	Meter        *usagemeter.Meter
	Proactive    *apns.ProactiveDelivery
	Device       *devicetools.Tools
	Google       *calendar.Google
	Brief        BriefComposer
	HTTP         *http.Client
	Log          *slog.Logger
}

// NewRunner builds a Runner.
func NewRunner(pool *pgxpool.Pool, opt RunnerOptions) *Runner {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	return &Runner{
		pool: pool, transport: opt.Transport, model: opt.DefaultModel, paths: opt.Paths, memories: opt.Memories,
		embeddings: opt.Embeddings, push: opt.APNS, meter: opt.Meter, proactive: opt.Proactive, device: opt.Device,
		google: opt.Google, brief: opt.Brief, http: opt.HTTP, guard: capGuard{pool: pool, now: time.Now}, log: opt.Log, now: time.Now,
	}
}

// runResult is SkillRunResult.
type runResult struct {
	runID              uuid.UUID
	status             string // "ok" | "error"
	markdown           string
	blocks             *[]luminaBlock
	err                *string
	modelUsed          *string
	mtokIn, mtokOut    int
	startedAt, endedAt time.Time
}

// runUser is what the controller already loaded for the caller. trigger is
// SkillTrigger's description in the agent prompt: "" is .manual, the HTTP routes'
// trigger; CronScheduler passes "cron".
type runUser struct {
	tenant                   uuid.UUID
	tier, override, username string
	trigger                  string
}

// Run is SkillRunner.run. The trigger is user.trigger, .manual when empty.
func (r *Runner) Run(ctx context.Context, mf skills.Manifest, user runUser, input *string, arguments map[string]string) (runResult, error) {
	started := r.now()
	runID := uuid.New()
	tenant := user.tenant
	if err := r.guard.checkAndIncrement(ctx, tenant, user.tier, mf); err != nil {
		return runResult{}, err
	}
	if r.meter != nil {
		if d := r.meter.CheckSkillBudget(ctx, tenant, mf.Name); d.Action == usagemeter.BudgetDeny {
			_ = r.guard.recordFailure(ctx, tenant, mf)
			return runResult{}, &llm.UsageCapExceededError{RetryAfter: d.RetryAfter}
		}
	}

	res := runResult{runID: runID, startedAt: started}
	// Routing context: the user row (Swift's User.find, nil when gone) and the
	// Cerberus scope: a job- skill is a job, anything else a skill.
	scope := &llm.CerberusScope{Surface: llm.SurfaceSkill, SpaceID: r.stateSpace(ctx, tenant, mf)}
	if strings.HasPrefix(mf.Name, "job-") {
		scope.Surface, scope.JobID = llm.SurfaceJob, mf.Name
	}
	routed := llm.WithValues(ctx, func(v *llm.Values) {
		if u, ok := r.routingUser(ctx, tenant); ok {
			v.CurrentUser = u
		}
		v.CerberusScope = scope
	})

	summary, blocks, err := r.runAgent(routed, mf, tenant, user.trigger, input, arguments, &res)
	if err == nil {
		err = r.dispatchOutputs(ctx, mf, tenant, user.username, summary)
	}
	if err == nil {
		ok := res
		ok.status, ok.markdown, ok.blocks, ok.endedAt = "ok", summary, blocks, r.now()
		if err = r.persist(ctx, ok, mf, tenant); err == nil {
			return ok, nil
		}
	}
	// Swift's catch: refund the slot, log, record the failed run, rethrow.
	_ = r.guard.recordFailure(ctx, tenant, mf)
	message := err.Error()
	r.log.ErrorContext(ctx, fmt.Sprintf("skills.runner run failed skill=%s tenant=%s: %s", mf.Name, ids.Upper(tenant), message))
	res.status, res.markdown, res.err, res.endedAt = "error", "", &message, r.now()
	_ = r.persist(ctx, res, mf, tenant)
	return runResult{}, err
}

func (r *Runner) routingUser(ctx context.Context, tenant uuid.UUID) (*llm.User, bool) {
	u := llm.User{ID: tenant}
	if err := r.pool.QueryRow(ctx, `SELECT tier, tier_override, privacy_no_cn_origin FROM users WHERE id = $1`, tenant).
		Scan(&u.Tier, &u.TierOverride, &u.PrivacyNoCNOrigin); err != nil {
		return nil, false
	}
	return &u, true
}

func (r *Runner) stateSpace(ctx context.Context, tenant uuid.UUID, mf skills.Manifest) *uuid.UUID {
	var space *uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT space_id FROM skills_state WHERE tenant_id = $1 AND source = $2 AND name = $3 LIMIT 1`,
		tenant, string(mf.Source), mf.Name).Scan(&space); err != nil {
		return nil
	}
	return space
}

// Agent loop

type toolFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function toolFunctionCall `json:"function"`
}

// agentMessage is SkillRunner.AgentMessage; nil fields are left out, as
// Swift's encodeIfPresent does.
type agentMessage struct {
	Role       string      `json:"role"`
	Content    *string     `json:"content,omitempty"`
	ToolCalls  *[]toolCall `json:"tool_calls,omitempty"`
	ToolCallID *string     `json:"tool_call_id,omitempty"`
	Name       *string     `json:"name,omitempty"`
}

type agentRequest struct {
	Model       string           `json:"model"`
	Messages    []agentMessage   `json:"messages"`
	Tools       []toolDefinition `json:"tools"`
	ToolChoice  string           `json:"tool_choice"`
	Temperature float64          `json:"temperature"`
	Stream      bool             `json:"stream"`
}

type agentResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message      agentMessage `json:"message"`
		FinishReason *string      `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     *int `json:"prompt_tokens,omitempty"`
		CompletionTokens *int `json:"completion_tokens,omitempty"`
	} `json:"usage,omitempty"`
}

const agentSystemPrompt = "You are running LuminaVault skill `%s`.\n" +
	"Follow the skill body. Tool availability is enforced server-side;\n" +
	"if a tool call returns an error, continue without trying to bypass it.\n" +
	"\n" +
	"OUTPUT FORMAT — choose ONE:\n" +
	"• Plain markdown / concise text (default), OR\n" +
	"• For data-rich results, a single JSON object:\n" +
	"  {\"markdown\":\"<short fallback>\",\"blocks\":[ ... ]}\n" +
	"  where each block is {\"type\": <kind>, ...fields}. Kinds:\n" +
	"  heading{text,level} · paragraph{text} · markdown{text} ·\n" +
	"  statCard{label,value,delta,trend:\"up|down|flat\"} ·\n" +
	"  lineChart{series:[{name,points:[{x,y}]}]} · barChart{series} ·\n" +
	"  list{items:[...]} · table{columns:[...],rows:[[...]]} ·\n" +
	"  badge{text} · keyValue{pairs:[{key,value}]} · quote{text} ·\n" +
	"  image{url} · divider. Pick the blocks that best present THIS\n" +
	"  domain (e.g. statCard+lineChart for metrics, table/list for\n" +
	"  collections, markdown for prose). Output ONLY the JSON, no fence."

var (
	errNoChoices    = wire.NewError(http.StatusBadGateway, "skill runner got no choices")
	errNotConverged = wire.NewError(http.StatusBadGateway, "skill runner agent did not converge")
)

func strPtr(s string) *string { return &s }

func (r *Runner) runAgent(ctx context.Context, mf skills.Manifest, tenant uuid.UUID, trigger string, input *string, arguments map[string]string, res *runResult) (string, *[]luminaBlock, error) {
	if trigger == "" {
		trigger = "manual"
	}
	allowed := map[string]bool{}
	for _, t := range mf.AllowedTools {
		allowed[t] = true
	}
	tools := []toolDefinition{}
	for _, t := range availableTools {
		if allowed[t.name] {
			tools = append(tools, t.definition())
		}
	}
	conversation := []agentMessage{
		{Role: "system", Content: strPtr(fmt.Sprintf(agentSystemPrompt, mf.Name))},
		{Role: "user", Content: strPtr("Trigger: " + trigger + "\n" + inputDescription(input, arguments) + "\n\nSkill body:\n" + mf.Body)},
	}
	for range 6 {
		payload, err := json.Marshal(agentRequest{
			Model: r.model, Messages: conversation, Tools: tools, ToolChoice: "auto", Temperature: 0.2,
		})
		if err != nil {
			return "", nil, err
		}
		meta, err := r.transport.ChatCompletions(ctx, payload, ids.Upper(tenant), "")
		if err != nil {
			return "", nil, err
		}
		var resp agentResponse
		if err := wire.DecodeInto(meta.Data, &resp); err != nil {
			return "", nil, err
		}
		res.modelUsed = &resp.Model
		in, out := usageDelta(meta.Headers, &resp)
		res.mtokIn += in
		res.mtokOut += out
		if (in > 0 || out > 0) && r.meter != nil {
			tag := "skill:" + mf.Name + "/" + resp.Model
			go r.meter.Record(context.WithoutCancel(ctx), tenant, tag, int64(in), int64(out))
		}
		if len(resp.Choices) == 0 {
			return "", nil, errNoChoices
		}
		assistant := resp.Choices[0].Message
		conversation = append(conversation, assistant)
		if assistant.ToolCalls != nil && len(*assistant.ToolCalls) > 0 {
			for _, call := range *assistant.ToolCalls {
				result := r.dispatch(ctx, tenant, allowed, call)
				conversation = append(conversation, agentMessage{
					Role: "tool", Content: strPtr(result), ToolCallID: strPtr(call.ID), Name: strPtr(call.Function.Name),
				})
			}
			continue
		}
		content := ""
		if assistant.Content != nil {
			content = *assistant.Content
		}
		summary, blocks := parseAgentOutput(content)
		return summary, blocks, nil
	}
	return "", nil, errNotConverged
}

// usageDelta is accumulateUsage: x-mtok-in style headers win over the body's usage.
func usageDelta(headers map[string]string, resp *agentResponse) (in, out int) {
	header := func(names ...string) (int, bool) {
		for _, n := range names {
			for k, v := range headers {
				if strings.EqualFold(k, n) {
					if i, err := strconv.Atoi(v); err == nil {
						return i, true
					}
				}
			}
		}
		return 0, false
	}
	if v, ok := header("x-mtok-in", "x-usage-mtok-in", "x-luminavault-mtok-in"); ok {
		in = v
	} else if resp.Usage != nil && resp.Usage.PromptTokens != nil {
		in = *resp.Usage.PromptTokens
	}
	if v, ok := header("x-mtok-out", "x-usage-mtok-out", "x-luminavault-mtok-out"); ok {
		out = v
	} else if resp.Usage != nil && resp.Usage.CompletionTokens != nil {
		out = *resp.Usage.CompletionTokens
	}
	return in, out
}

// inputDescription is SkillRunner.inputDescription.
func inputDescription(input *string, arguments map[string]string) string {
	var lines []string
	if input != nil && *input != "" {
		lines = append(lines, "Input: "+*input)
	}
	if len(arguments) > 0 {
		keys := make([]string, 0, len(arguments))
		for k := range arguments {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		rendered := make([]string, len(keys))
		for i, k := range keys {
			rendered[i] = k + "=" + arguments[k]
		}
		lines = append(lines, "Arguments:\n"+strings.Join(rendered, "\n"))
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n" + strings.Join(lines, "\n\n")
}

// parseAgentOutput is SkillRunner.parseAgentOutput: a {"markdown","blocks"} JSON
// object (optionally fenced) with at least one block, else the content as markdown.
func parseAgentOutput(content string) (string, *[]luminaBlock) {
	trimmed := swiftstr.TrimWhitespacesAndNewlines(content)
	unfenced := trimmed
	if strings.HasPrefix(trimmed, "```") {
		afterTag := strings.TrimLeft(trimmed, "`")
		if i := strings.IndexByte(afterTag, '\n'); i >= 0 {
			afterTag = afterTag[i+1:]
		} else {
			afterTag = ""
		}
		if end := strings.LastIndex(afterTag, "```"); end >= 0 {
			unfenced = afterTag[:end]
		} else {
			unfenced = afterTag
		}
	}
	if !strings.HasPrefix(unfenced, "{") {
		return content, nil
	}
	var env struct {
		Markdown *string        `json:"markdown,omitempty"`
		Blocks   *[]luminaBlock `json:"blocks,omitempty"`
	}
	if err := wire.DecodeInto([]byte(unfenced), &env); err != nil || env.Blocks == nil || len(*env.Blocks) == 0 {
		return content, nil
	}
	if env.Markdown != nil {
		return *env.Markdown, env.Blocks
	}
	return content, env.Blocks
}

// Outputs

var errChatNotWired = wire.NewError(http.StatusInternalServerError, "chat_message output is not wired on this server")

// dispatchOutputs is SkillRunner.dispatchOutputs for a manual run (no event
// payload, so memory_emit has no source file and vault_rewrite fails).
func (r *Runner) dispatchOutputs(ctx context.Context, mf skills.Manifest, tenant uuid.UUID, username, content string) error {
	// Wave 4: the daily brief becomes cards first; its chat message is then a
	// one-line pointer to them. A failed compose falls back to the full text.
	briefCards := -1
	if mf.Name == briefSkillName && r.brief != nil {
		n, err := r.brief.ComposeBrief(ctx, tenant, content, r.now())
		if err != nil {
			r.log.WarnContext(ctx, fmt.Sprintf("skills.runner brief compose failed tenant=%s: %v", ids.Upper(tenant), err))
		} else {
			briefCards = n
		}
	}
	for _, out := range mf.Outputs {
		switch out.Kind {
		case skills.OutputMemo:
			path := "memos/" + dateStamp(r.now()) + "/" + skillSlug(mf.Name) + ".md"
			if out.Path != nil {
				path = *out.Path
			}
			if err := r.persistVaultFile(ctx, tenant, path, content, "text/markdown", nil); err != nil {
				return err
			}
		case skills.OutputAPNSDigest:
			if err := r.push.NotifyDigest(ctx, tenant, username, content); err != nil {
				return err
			}
		case skills.OutputAPNSNudge:
			if err := r.push.NotifyNudge(ctx, tenant, username, content); err != nil {
				return err
			}
		case skills.OutputChatMessage:
			if r.proactive == nil {
				return errChatNotWired
			}
			if briefCards >= 0 {
				if briefCards == 0 {
					continue
				}
				if _, err := r.proactive.DeliverBrief(ctx, tenant, briefCards, mf.Name, r.now()); err != nil {
					return err
				}
				continue
			}
			if swiftstr.TrimWhitespacesAndNewlines(content) == "" {
				continue
			}
			if _, err := r.proactive.Deliver(ctx, tenant, content, mf.Name); err != nil {
				return err
			}
		case skills.OutputMemoryEmit:
			if _, err := r.persistMemory(ctx, tenant, content, nil); err != nil {
				return err
			}
		case skills.OutputVaultRewrite:
			return wire.NewError(http.StatusBadRequest, "vault_rewrite output requires source vault file id")
		}
	}
	return nil
}

// persistMemory keeps the run's output even when embedding is down.
func (r *Runner) persistMemory(ctx context.Context, tenant uuid.UUID, content string, source *uuid.UUID) (*memorystore.Memory, error) {
	vec, err := embedding.EmbedForWrite(ctx, r.embeddings, content, tenant, "skill.memory")
	if err != nil {
		return nil, err
	}
	return r.memories.Create(ctx, memorystore.CreateParams{Tenant: tenant, Content: content, Embedding: vec, SourceVaultFileID: source})
}

var errInvalidVaultPath = wire.NewError(http.StatusBadRequest, "invalid vault path")

// validateRelativePath is SkillRunner.validateRelativePath.
func validateRelativePath(path string) (string, error) {
	trimmed := swiftstr.TrimWhitespacesAndNewlines(path)
	if trimmed == "" || strings.HasPrefix(trimmed, "/") {
		return "", errInvalidVaultPath
	}
	for _, part := range strings.Split(trimmed, "/") {
		if part == ".." {
			return "", errInvalidVaultPath
		}
	}
	return trimmed, nil
}

// persistVaultFile is SkillRunner.persistVaultFile: write the raw file, then
// update the row at that path (processed_at cleared) or insert one.
func (r *Runner) persistVaultFile(ctx context.Context, tenant uuid.UUID, path, content, contentType string, spaceID *uuid.UUID) error {
	safe, err := validateRelativePath(path)
	if err != nil {
		return err
	}
	if err := r.paths.EnsureTenantDirectories(tenant); err != nil {
		return err
	}
	target := filepath.Join(r.paths.Raw(tenant), safe)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // Foundation's createDirectory default
		return err
	}
	data := []byte(content)
	if err := vaultfiles.WriteAtomic(target, data); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	var id uuid.UUID
	err = r.pool.QueryRow(ctx, `SELECT id FROM vault_files WHERE tenant_id = $1 AND path = $2 LIMIT 1`, tenant, safe).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = r.pool.Exec(ctx, `INSERT INTO vault_files (id, tenant_id, space_id, path, content_type, size_bytes, sha256, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())`, uuid.New(), tenant, spaceID, safe, contentType, len(data), digest)
		return err
	case err != nil:
		return err
	}
	_, err = r.pool.Exec(ctx, `UPDATE vault_files SET size_bytes = $2, sha256 = $3, content_type = $4, processed_at = NULL,
		space_id = COALESCE($5, space_id), updated_at = NOW() WHERE id = $1`, id, len(data), digest, contentType, spaceID)
	return err
}

// persist writes skill_run_log, then skills_state's last_* columns, then files a
// job's output into its Space.
func (r *Runner) persist(ctx context.Context, res runResult, mf skills.Manifest, tenant uuid.UUID) error {
	var blocks *string
	if res.blocks != nil {
		if b, err := json.Marshal(*res.blocks); err == nil {
			s := string(b)
			blocks = &s
		}
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO skill_run_log
		(id, tenant_id, source, name, started_at, ended_at, status, error, model_used, mtok_in, mtok_out, markdown, blocks)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb)`,
		res.runID, tenant, string(mf.Source), mf.Name, res.startedAt, res.endedAt, res.status, res.err, res.modelUsed,
		res.mtokIn, res.mtokOut, res.markdown, blocks); err != nil {
		return err
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO skills_state (tenant_id, source, name, enabled, last_run_at, last_status, last_error)
		VALUES ($1, $2, $3, TRUE, $4, $5, $6)
		ON CONFLICT (tenant_id, source, name) DO UPDATE
		    SET last_run_at = EXCLUDED.last_run_at, last_status = EXCLUDED.last_status, last_error = EXCLUDED.last_error`,
		tenant, string(mf.Source), mf.Name, res.endedAt, res.status, res.err); err != nil {
		return err
	}
	r.fileJobResult(ctx, res, mf, tenant)
	return nil
}

// fileJobResult is fileJobResultIfNeeded: best effort, never fails the run.
func (r *Runner) fileJobResult(ctx context.Context, res runResult, mf skills.Manifest, tenant uuid.UUID) {
	if res.status != "ok" || swiftstr.TrimWhitespacesAndNewlines(res.markdown) == "" {
		return
	}
	var space *uuid.UUID
	var slug *string
	if err := r.pool.QueryRow(ctx, `SELECT ss.space_id, s.slug FROM skills_state ss LEFT JOIN spaces s ON s.id = ss.space_id
		WHERE ss.tenant_id = $1 AND ss.source = $2 AND ss.name = $3`, tenant, string(mf.Source), mf.Name).Scan(&space, &slug); err != nil ||
		space == nil || slug == nil {
		return
	}
	path := *slug + "/jobs/" + mf.Name + "-" + dateStamp(res.endedAt) + ".md"
	_ = r.persistVaultFile(ctx, tenant, path, res.markdown, "text/markdown", space)
}

// dateStamp is yyyy-MM-dd in UTC.
func dateStamp(t time.Time) string { return t.UTC().Format("2006-01-02") }

// skillSlug is SkillRunner.slug.
func skillSlug(value string) string {
	var out []string
	for _, ch := range swiftstr.Characters(strings.ToLower(value)) {
		switch {
		case swiftstr.IsLetterOrNumber(ch):
			out = append(out, ch)
		case ch == " " || ch == "-" || ch == "_":
			if len(out) > 0 && out[len(out)-1] != "-" {
				out = append(out, "-")
			}
		}
	}
	for len(out) > 0 && out[len(out)-1] == "-" {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return "skill"
	}
	return strings.Join(out[:min(64, len(out))], "")
}
