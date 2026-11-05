package grok

// Hand-written request types, in Swift's declaration order (GrokDTOs.swift): Swift
// reports the first failing key in that order and wire.Bind walks fields in struct
// order, where the generated models are alphabetical. Responses use the generated
// models.

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is GrokChatRequest. stream is decoded, so a wrong type is a 400, and
// never sent on: HermesGrokProxy's ChatCompletionsRequest has no such field.
type chatRequest struct {
	Messages  []chatMessage `json:"messages"`
	Model     *string       `json:"model,omitempty"`
	Stream    *bool         `json:"stream,omitempty"`
	MaxTokens *int          `json:"maxTokens,omitempty"`
}

type xSearchRequest struct {
	Query                    string    `json:"query"`
	AllowedXHandles          *[]string `json:"allowedXHandles,omitempty"`
	ExcludedXHandles         *[]string `json:"excludedXHandles,omitempty"`
	FromDate                 *string   `json:"fromDate,omitempty"`
	ToDate                   *string   `json:"toDate,omitempty"`
	EnableImageUnderstanding *bool     `json:"enableImageUnderstanding,omitempty"`
	EnableVideoUnderstanding *bool     `json:"enableVideoUnderstanding,omitempty"`
}

type visionRequest struct {
	Prompt    string   `json:"prompt"`
	ImageURLs []string `json:"imageURLs"`
}

// Upstream payloads: the proxy's JSONEncoder with .convertToSnakeCase, which omits nil.

type upstreamChat struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens *int          `json:"max_tokens,omitempty"`
}

type upstreamResponses struct {
	Model string         `json:"model"`
	Input string         `json:"input"`
	Tools []upstreamTool `json:"tools"`
}

type upstreamTool struct {
	Type    string          `json:"type"`
	XSearch upstreamXSearch `json:"x_search"`
}

// The pointers to slices keep an empty list apart from an absent one.
type upstreamXSearch struct {
	AllowedXHandles          *[]string `json:"allowed_x_handles,omitempty"`
	ExcludedXHandles         *[]string `json:"excluded_x_handles,omitempty"`
	FromDate                 *string   `json:"from_date,omitempty"`
	ToDate                   *string   `json:"to_date,omitempty"`
	EnableImageUnderstanding *bool     `json:"enable_image_understanding,omitempty"`
	EnableVideoUnderstanding *bool     `json:"enable_video_understanding,omitempty"`
}

type upstreamVision struct {
	Model    string          `json:"model"`
	Messages []visionMessage `json:"messages"`
}

type visionMessage struct {
	Role    string       `json:"role"`
	Content []visionPart `json:"content"`
}

type visionPart struct {
	Type     string       `json:"type"`
	Text     *string      `json:"text,omitempty"`
	ImageURL *visionImage `json:"image_url,omitempty"`
}

type visionImage struct {
	URL string `json:"url"`
}

// Upstream replies, after decodeUpstream has camelCased the keys. Fields wire.Decode
// treats as required are the non-optional ones in HermesGrokProxy.

type chatCompletion struct {
	Model   *string      `json:"model,omitempty"`
	Choices []chatChoice `json:"choices"`
	Usage   *GrokUsage   `json:"usage,omitempty"`
}

type chatChoice struct {
	Message chatReplyMessage `json:"message"`
}

type chatReplyMessage struct {
	Content string `json:"content"`
}

type responsesReply struct {
	Model     *string               `json:"model,omitempty"`
	Output    *string               `json:"output,omitempty"`
	Citations []GrokXSearchCitation `json:"citations,omitempty"`
}
