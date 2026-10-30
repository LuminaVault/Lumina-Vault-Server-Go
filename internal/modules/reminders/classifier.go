package reminders

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/wire"
	"strings"
	"time"
)

// ChatTransport is the slice of Swift's HermesChatTransport the classifier
// uses: an OpenAI-shaped chat-completions request in, the raw response out.
type ChatTransport interface {
	ChatCompletions(ctx context.Context, payload []byte, sessionKey string) ([]byte, error)
}

// ErrTransportUnavailable is what UnavailableTransport returns.
var ErrTransportUnavailable = errors.New("LLM routing is not ported to Go")

// UnavailableTransport fails every call, so the classifier fails closed
// (isReminder: false). It is what cmd/App wires until Swift's
// RoutedLLMTransport (provider registry, Cerberus routing, the Haiku trial,
// usage metering) is ported. In the parity environment Swift's transport has
// no reachable provider either, so both answer {"isReminder":false}.
type UnavailableTransport struct{}

func (UnavailableTransport) ChatCompletions(context.Context, []byte, string) ([]byte, error) {
	return nil, ErrTransportUnavailable
}

// Classifier is ReminderIntentClassifier (HER-55).
type Classifier struct {
	Transport ChatTransport
	Model     string
}

type chatMessage struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}

// SystemPrompt is the classifier's system message, byte-identical to Swift's.
func SystemPrompt(now time.Time) string {
	nowString := now.UTC().Format("2006-01-02T15:04:05Z")
	return "You classify whether a user's message is a request to BE REMINDED of " +
		"something at a time — a one-shot nudge (\"remind me to call mom at 5pm\", " +
		"\"ping me in an hour\") or a recurring nudge (\"remind me to stretch every " +
		"weekday at 3\"). This is distinct from a recurring JOB that produces a " +
		"report/digest each run — those are NOT reminders.\n" +
		"\n" +
		"The current time is " + nowString + " (UTC). Resolve all relative phrasing " +
		"against it and output absolute UTC times.\n" +
		"\n" +
		"Respond with ONLY a JSON object (no prose, no code fence):\n" +
		"{\"isReminder\": true|false,\n" +
		" \"title\": \"<short imperative title, e.g. Call mom>\",\n" +
		" \"body\": \"<optional extra detail, may be empty>\",\n" +
		" \"fireAt\": \"<ISO 8601 UTC, e.g. 2026-06-09T17:00:00Z>\",\n" +
		" \"recurrenceCron\": \"<crontab if recurring, else omit>\",\n" +
		" \"scheduleHuman\": \"<e.g. Tomorrow at 5:00 PM, or Every weekday at 3:00 PM>\"}\n" +
		"\n" +
		"If it is NOT a reminder request, return exactly {\"isReminder\": false}.\n" +
		"For recurring reminders, infer a sensible cron (\"every weekday at 3\" → " +
		"0 15 * * 1-5) and still set fireAt to the next matching occurrence."
}

// Payload is the chat-completions body Swift sends (temperature 0.1,
// stream false). Swift's transport adapters, not the classifier, strip
// parameters a provider rejects.
func (c Classifier) Payload(text string, now time.Time) ([]byte, error) {
	system := SystemPrompt(now)
	return json.Marshal(chatRequest{
		Model:       c.Model,
		Messages:    []chatMessage{{Role: "system", Content: &system}, {Role: "user", Content: &text}},
		Temperature: 0.1,
		Stream:      false,
	})
}

// Classify never fails: any transport or parse problem is isReminder false.
func (c Classifier) Classify(ctx context.Context, text, sessionKey string, now time.Time) ReminderProposalDTO {
	notReminder := ReminderProposalDTO{IsReminder: false}
	if c.Transport == nil {
		return notReminder
	}
	payload, err := c.Payload(text, now)
	if err != nil {
		return notReminder
	}
	raw, err := c.Transport.ChatCompletions(ctx, payload, sessionKey)
	if err != nil {
		return notReminder
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &resp) != nil || len(resp.Choices) == 0 || resp.Choices[0].Message.Content == nil {
		return notReminder
	}
	return ParseProposal(*resp.Choices[0].Message.Content)
}

// ParseProposal is ReminderIntentClassifier.parse: tolerate a ```json fence;
// anything unparseable, not a reminder, or with an unparseable fireAt is
// isReminder false. body defaults to "" and an empty cron becomes nil.
func ParseProposal(content string) ReminderProposalDTO {
	notReminder := ReminderProposalDTO{IsReminder: false}
	trimmed := strings.TrimSpace(content)
	unfenced := trimmed
	if strings.HasPrefix(trimmed, "```") {
		afterTicks := strings.TrimLeft(trimmed, "`")
		afterTag := ""
		if nl := strings.IndexByte(afterTicks, '\n'); nl >= 0 {
			afterTag = afterTicks[nl+1:]
		}
		if end := strings.LastIndex(afterTag, "```"); end >= 0 {
			unfenced = afterTag[:end]
		} else {
			unfenced = afterTag
		}
	}
	var env struct {
		IsReminder     *bool   `json:"isReminder"`
		Title          *string `json:"title"`
		Body           *string `json:"body"`
		FireAt         *string `json:"fireAt"`
		RecurrenceCron *string `json:"recurrenceCron"`
		ScheduleHuman  *string `json:"scheduleHuman"`
	}
	if json.Unmarshal([]byte(unfenced), &env) != nil || env.IsReminder == nil || !*env.IsReminder || env.FireAt == nil {
		return notReminder
	}
	fireAt, err := time.Parse(time.RFC3339Nano, *env.FireAt)
	if err != nil {
		return notReminder
	}
	body := ""
	if env.Body != nil {
		body = *env.Body
	}
	var cron *string
	if env.RecurrenceCron != nil && *env.RecurrenceCron != "" {
		cron = env.RecurrenceCron
	}
	return ReminderProposalDTO{
		IsReminder:     true,
		Title:          env.Title,
		Body:           &body,
		FireAt:         &wire.Time{Time: fireAt},
		RecurrenceCron: cron,
		ScheduleHuman:  env.ScheduleHuman,
	}
}
