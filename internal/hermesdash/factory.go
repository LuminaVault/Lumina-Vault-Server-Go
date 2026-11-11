package hermesdash

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/hermescontainers"
	"lumina/internal/hermesresolve"
	"lumina/internal/ids"
	"lumina/internal/secretbox"
	"lumina/internal/ssrfguard"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Credentials is HermesDashboardCredentialStore.Credentials.
type Credentials struct {
	URL   string
	Token string
}

// DashboardCredentials is HermesDashboardCredentialStore.credentials: the
// sealed cron_dashboard_* columns of user_hermes_config. nil when not set; an
// error when the token does not open.
func DashboardCredentials(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box, tenant uuid.UUID) (*Credentials, error) {
	var dashURL *string
	var ct, nonce []byte
	err := pool.QueryRow(ctx, `SELECT cron_dashboard_url, cron_dashboard_token_ciphertext, cron_dashboard_token_nonce
		FROM user_hermes_config WHERE tenant_id = $1 LIMIT 1`, tenant).Scan(&dashURL, &ct, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if dashURL == nil || *dashURL == "" || ct == nil || nonce == nil {
		return nil, nil
	}
	token, err := box.Open(secretbox.Sealed{Ciphertext: ct, Nonce: nonce}, tenant)
	if err != nil {
		return nil, err
	}
	return &Credentials{URL: *dashURL, Token: token}, nil
}

// Provider is HermesMirrorTransportProviding: the seam tests replace.
type Provider interface {
	Kind(ctx context.Context, tenant uuid.UUID) TransportKind
	Transport(ctx context.Context, tenant uuid.UUID) (Transport, error)
}

// Factory is HermesMirrorTransportFactory: a BYO gateway or dashboard
// credentials pick Remote; otherwise Filesystem on the per-tenant container
// volume when one exists, else the managed Hermes home, with sessions over the
// tenant's gateway.
type Factory struct {
	Pool                  *pgxpool.Pool
	Box                   *secretbox.Box
	Guard                 ssrfguard.Guard
	Resolver              *hermesresolve.Resolver
	Containers            *hermescontainers.Manager
	PerTenantDataRootBase string
	ManagedHermesRoot     string
	HTTP                  Doer
}

// Kind is the cheap classification: one row read plus the resolver.
func (f *Factory) Kind(ctx context.Context, tenant uuid.UUID) TransportKind {
	if creds, err := DashboardCredentials(ctx, f.Pool, f.Box, tenant); err == nil && creds != nil {
		return KindRemote
	}
	res, err := f.Resolver.Resolve(ctx, tenant)
	if err != nil {
		slog.WarnContext(ctx, "byo hermes gateway is configured but unusable", "tenant", ids.Upper(tenant), "error", err.Error())
		return KindNone
	}
	if res.IsUserOverride {
		return KindRemote
	}
	return KindManaged
}

// Transport builds the tenant's transport. A configured gateway that won't
// resolve is an error, unless a dashboard is configured too.
func (f *Factory) Transport(ctx context.Context, tenant uuid.UUID) (Transport, error) {
	dashboard, err := DashboardCredentials(ctx, f.Pool, f.Box, tenant)
	if err != nil {
		return nil, err
	}
	var own *hermesresolve.Resolution
	res, err := f.Resolver.Resolve(ctx, tenant)
	if err != nil {
		if dashboard == nil {
			return nil, err
		}
		slog.WarnContext(ctx, "byo hermes gateway unusable; continuing over the dashboard", "tenant", ids.Upper(tenant), "error", err.Error())
	} else if res.IsUserOverride {
		own = &res
	}
	if dashboard == nil && own == nil {
		return f.managed(ctx, tenant), nil
	}
	remote := &Remote{SkillsHTTP: f.HTTP}
	if own != nil {
		remote.GatewayBaseURL = own.BaseURL
		remote.GatewayAuthHeader = own.AuthHeader
		remote.Gateway = NewGateway(own.BaseURL, own.AuthHeader, f.HTTP)
	}
	if dashboard != nil {
		remote.Dashboard = NewDashboard(dashboard.URL, dashboard.Token, f.Guard, f.HTTP)
	}
	return remote, nil
}

func (f *Factory) managed(ctx context.Context, tenant uuid.UUID) *Filesystem {
	root := f.ManagedHermesRoot
	var sessions *Gateway
	if f.Containers != nil {
		if handle, err := f.Containers.Handle(ctx, tenant); err == nil && handle != nil {
			volume := f.PerTenantDataRootBase + "/" + strings.ToLower(tenant.String())
			if _, err := os.Stat(volume); err == nil {
				root = volume
			}
			if base, err := url.Parse(handle.BaseURL()); err == nil {
				auth := "Bearer " + handle.APIServerKey
				sessions = NewGateway(base, &auth, f.HTTP)
			}
		}
	}
	if sessions == nil {
		if res, err := f.Resolver.Resolve(ctx, tenant); err == nil {
			sessions = NewGateway(res.BaseURL, res.AuthHeader, f.HTTP)
		}
	}
	return NewFilesystem(root, sessions)
}
