package connectedapps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxOAuthBody bounds every OAuth response body read.
const maxOAuthBody = 64 << 10

// asMeta is the slice of RFC 8414 authorization-server metadata we use.
type asMeta struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

// send validates rawURL with the guard, performs the request and returns the
// status and a size-bounded body. Errors never carry the body or the form.
func (o *OAuth) send(ctx context.Context, method, rawURL, contentType string, body []byte) (int, []byte, error) {
	if o.http == nil || o.guard == nil {
		return 0, nil, errors.New("connectedapps: oauth client not configured")
	}
	u, err := o.guard.Validate(ctx, rawURL)
	if err != nil {
		return 0, nil, fmt.Errorf("connectedapps: url rejected: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("connectedapps: bad oauth request")
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		// url.Error embeds the URL only, but unwrap to be safe about contents.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, fmt.Errorf("connectedapps: oauth request failed: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBody))
	if err != nil {
		return 0, nil, fmt.Errorf("connectedapps: oauth read failed: %w", err)
	}
	return resp.StatusCode, b, nil
}

func (o *OAuth) getJSON(ctx context.Context, rawURL string, out any) error {
	status, b, err := o.send(ctx, http.MethodGet, rawURL, "", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("connectedapps: metadata fetch returned %d", status)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return errors.New("connectedapps: metadata is not valid JSON")
	}
	return nil
}

// discover finds the authorization server for an app's MCP endpoint: the
// protected-resource document, then the AS metadata, whose issuer must equal
// the authorization_servers entry it was fetched from (RFC 8414 3.3).
func (o *OAuth) discover(ctx context.Context, app App) (asMeta, error) {
	mcp, err := url.Parse(app.MCPURL)
	if err != nil || mcp.Scheme == "" || mcp.Host == "" {
		return asMeta{}, errors.New("connectedapps: bad MCP url")
	}
	prm := mcp.Scheme + "://" + mcp.Host + "/.well-known/oauth-protected-resource" + strings.TrimRight(mcp.Path, "/")
	var pr struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := o.getJSON(ctx, prm, &pr); err != nil {
		return asMeta{}, err
	}
	if len(pr.AuthorizationServers) == 0 || pr.AuthorizationServers[0] == "" {
		return asMeta{}, errors.New("connectedapps: no authorization server advertised")
	}
	issuer := pr.AuthorizationServers[0]
	var meta asMeta
	if err := o.getJSON(ctx, strings.TrimRight(issuer, "/")+"/.well-known/oauth-authorization-server", &meta); err != nil {
		return asMeta{}, err
	}
	if meta.Issuer != issuer {
		return asMeta{}, errors.New("connectedapps: issuer mismatch")
	}
	return meta, nil
}

// clientID returns the cached DCR client id for the app, registering a public
// client (no secret) on first use.
func (o *OAuth) clientID(ctx context.Context, app App, meta asMeta) (string, error) {
	var id string
	err := o.pool.QueryRow(ctx, `SELECT client_id FROM connected_app_clients WHERE app_id = $1`, app.ID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !isNoRows(err) {
		return "", err
	}
	if meta.RegistrationEndpoint == "" {
		return "", errors.New("connectedapps: authorization server has no registration endpoint")
	}
	reqBody, _ := json.Marshal(map[string]any{
		"client_name":                "LuminaVault",
		"redirect_uris":              []string{o.callbackURL},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	status, b, err := o.send(ctx, http.MethodPost, meta.RegistrationEndpoint, "application/json", reqBody)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", fmt.Errorf("connectedapps: registration returned %d", status)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if json.Unmarshal(b, &reg) != nil || reg.ClientID == "" {
		return "", errors.New("connectedapps: registration returned no client_id")
	}
	// Two concurrent first starts may both register; the first row wins.
	if _, err := o.pool.Exec(ctx, `
INSERT INTO connected_app_clients (app_id, client_id) VALUES ($1, $2)
ON CONFLICT (app_id) DO NOTHING`, app.ID, reg.ClientID); err != nil {
		return "", err
	}
	if err := o.pool.QueryRow(ctx, `SELECT client_id FROM connected_app_clients WHERE app_id = $1`, app.ID).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}
