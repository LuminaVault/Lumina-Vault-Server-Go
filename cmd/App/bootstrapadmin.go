package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/authservice"
	"lumina/internal/config"
	"lumina/internal/db"
	"lumina/internal/hermesprofile"
	"lumina/internal/ids"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultpaths"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BootstrapAdminError's cases, with Swift's descriptions.
var (
	errBootstrapMissingCredentials = errors.New("bootstrap-admin requires BOOTSTRAP_ADMIN_EMAIL and BOOTSTRAP_ADMIN_PASSWORD")
	errBootstrapJWTSecretMissing   = errors.New("bootstrap-admin requires JWT_HMAC_SECRET to boot AuthService")
	errBootstrapUserLookupFailed   = errors.New("bootstrap-admin: register completed but user lookup failed")
)

// bootstrapAdminSummary is BootstrapAdminSummary, printed as one JSON object so
// setup.sh can capture the ids.
type bootstrapAdminSummary struct {
	Email      string
	UserID     string
	Username   string
	Created    bool
	IsAdmin    bool
	IsVerified bool
}

// bootstrapAdmin is runBootstrapAdminCommand (HER-30): seeds or promotes the
// first admin. A known email is promoted in place; a new one goes through the
// normal register (trial, personal vault, soft Hermes profile) and is then
// promoted. Idempotent. Swift weakness, kept: the summary goes to stdout, and
// the password's only check is register's.
func bootstrapAdmin(ctx context.Context, cfg config.Config, out io.Writer) error {
	log := slog.Default().With("label", "lv.cli.bootstrap-admin")
	username := swiftstr.Lowercased(swiftstr.TrimWhitespaces(cfg.BootstrapAdminUsername))
	if cfg.BootstrapAdminEmail == "" || cfg.BootstrapAdminPassword == "" {
		return errBootstrapMissingCredentials
	}
	email := swiftstr.Lowercased(cfg.BootstrapAdminEmail)

	pool, err := db.Open(ctx, db.DSN(cfg))
	if err != nil {
		return err
	}
	defer pool.Close()

	summary, err := provisionAdmin(ctx, pool, cfg, email, username, log)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, summary.prettyJSON()+"\n")
	return err
}

func provisionAdmin(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, email, username string, log *slog.Logger) (bootstrapAdminSummary, error) {
	// The repository lookup needs no keys; Swift builds AuthService only to register.
	lookup := authservice.New(pool, authservice.Options{})
	existing, err := lookup.FindUserByEmail(ctx, email)
	if err != nil {
		return bootstrapAdminSummary{}, err
	}
	if existing != nil {
		s, err := promoteAdmin(ctx, pool, existing.ID)
		if err != nil {
			return bootstrapAdminSummary{}, err
		}
		log.Info(fmt.Sprintf("bootstrap-admin existing user promoted email=%s userID=%s", email, s.UserID))
		return s, nil
	}

	if cfg.JWTHMACSecret == "" {
		return bootstrapAdminSummary{}, errBootstrapJWTSecretMissing
	}
	kid := cfg.JWTKID
	if kid == "" {
		kid = "lv-default"
	}
	paths := vaultpaths.Paths{Root: cfg.VaultRootPath}
	service := authservice.New(pool, authservice.Options{
		Keys:     auth.NewKeyset(auth.Key{Kid: kid, Secret: []byte(cfg.JWTHMACSecret)}),
		Profiles: hermesprofile.New(pool, hermesprofile.NewGateway(cfg.HermesGatewayKind, cfg.HermesDataRoot), paths),
		Log:      log,
	})
	if _, err := service.Register(ctx, email, username, cfg.BootstrapAdminPassword); err != nil {
		return bootstrapAdminSummary{}, err
	}
	user, err := lookup.FindUserByEmail(ctx, email)
	if err != nil {
		return bootstrapAdminSummary{}, err
	}
	if user == nil {
		return bootstrapAdminSummary{}, errBootstrapUserLookupFailed
	}
	s, err := promoteAdmin(ctx, pool, user.ID)
	if err != nil {
		return bootstrapAdminSummary{}, err
	}
	s.Created = true
	log.Info(fmt.Sprintf("bootstrap-admin created admin user email=%s userID=%s", email, s.UserID))
	return s, nil
}

// promoteAdmin is promote: sets is_admin and is_verified, saving (and so
// touching updated_at) only when one of them changes.
func promoteAdmin(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) (bootstrapAdminSummary, error) {
	if _, err := pool.Exec(ctx, `UPDATE users SET is_admin = true, is_verified = true, updated_at = NOW()
		WHERE id = $1 AND (NOT is_admin OR NOT is_verified)`, id); err != nil {
		return bootstrapAdminSummary{}, err
	}
	s := bootstrapAdminSummary{UserID: ids.Upper(id)}
	err := pool.QueryRow(ctx, `SELECT email, username, is_admin, is_verified FROM users WHERE id = $1`, id).
		Scan(&s.Email, &s.Username, &s.IsAdmin, &s.IsVerified)
	return s, err
}

// prettyJSON is JSONEncoder with [.prettyPrinted, .sortedKeys]: two-space
// indent and Foundation's `"key" : value` separator.
func (s bootstrapAdminSummary) prettyJSON() string {
	str := func(v string) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	fields := []string{
		fmt.Sprintf(`"created" : %t`, s.Created),
		`"email" : ` + str(s.Email),
		fmt.Sprintf(`"isAdmin" : %t`, s.IsAdmin),
		fmt.Sprintf(`"isVerified" : %t`, s.IsVerified),
		`"userID" : ` + str(s.UserID),
		`"username" : ` + str(s.Username),
	}
	return "{\n  " + strings.Join(fields, ",\n  ") + "\n}"
}
