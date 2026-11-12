package authservice

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/wire"
	"net/http"
)

// XUsersMeURL is the URL DefaultXAPIClient hard-codes.
const XUsersMeURL = "https://api.x.com/2/users/me?user.fields=id,name,username,verified,verified_email"

// XUser is XUserResponse.XUserData: id, name and username required.
type XUser struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Username string  `json:"username"`
	Email    *string `json:"email,omitempty"`
	Verified *bool   `json:"verified,omitempty"`
}

// XUserResponse is the /2/users/me envelope.
type XUserResponse struct {
	Data XUser `json:"data"`
}

// DecodeXUserResponse decodes as JSONDecoder does XUserResponse. Its error is
// a plain one, not an HTTP 400: in Swift a DecodingError from the upstream
// body escapes the handler as a 500.
func DecodeXUserResponse(body []byte) (*XUser, error) {
	var out XUserResponse
	if err := wire.DecodeInto(body, &out); err != nil {
		return nil, fmt.Errorf("x users/me: %s", err.Error())
	}
	return &out.Data, nil
}

// XClient is XAPIClient: verify a bearer access_token against /2/users/me.
type XClient interface {
	FetchMe(ctx context.Context, accessToken string) (*XUser, error)
}

// HTTPXClient is DefaultXAPIClient.
type HTTPXClient struct {
	URL    string // XUsersMeURL; tests point it at a stub
	Client *http.Client
	Log    *slog.Logger
}

// NewXClient returns DefaultXAPIClient on the real URL.
func NewXClient(log *slog.Logger) *HTTPXClient {
	return &HTTPXClient{URL: XUsersMeURL, Client: &http.Client{Timeout: fetchTimeout}, Log: log}
}

var errXRejected = wire.NewError(http.StatusUnauthorized, "x access_token rejected")

// FetchMe is DefaultXAPIClient.fetchMe: a non-2xx answer is 401 "x access_token
// rejected"; a transport error escapes as a 500.
func (x *HTTPXClient) FetchMe(ctx context.Context, accessToken string) (*XUser, error) {
	req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), http.MethodGet, x.URL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	client := x.Client
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview := body
		if len(preview) > 512 {
			preview = preview[:512]
		}
		x.log().Error(fmt.Sprintf("x users/me %d: %s", resp.StatusCode, preview))
		return nil, errXRejected
	}
	return DecodeXUserResponse(body)
}

func (x *HTTPXClient) log() *slog.Logger {
	if x.Log == nil {
		return slog.Default()
	}
	return x.Log
}
