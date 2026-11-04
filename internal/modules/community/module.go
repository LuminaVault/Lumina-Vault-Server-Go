// Package community serves /v1/community and /v1/admin/community: the Go port
// of LuminaVaultServer Sources/App/Community (CommunityController.swift,
// CommunityService.swift), the global board every signed-in user shares across
// vaults. Every query is cross-tenant, so the rule that keeps vault data out is
// structural: responses are built only from the post projection in queries.sql
// (posts joined to profiles) and never from users.
//
// COMMUNITY_ENABLED off: /me says enabled:false, every other /v1/community route
// is 503, and /v1/admin/community does not exist. COMMUNITY_POSTING_ENABLED off:
// threads and replies are 503, everything else works.
//
// Each handler runs Swift's checks in Swift's order: the switch, then for most
// routes the body, then identity, then path ids. DailyPostWorker stays in Swift.
// It publishes the rows these routes read and sweeps posts held pending.
package community

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/community/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/community/db"
	"lumina/internal/ratelimit"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	// TermsVersion: bump when the Community Guidelines change; members
	// re-accept before they can post again.
	TermsVersion = 1
	// aiLabel is the caption every AI post carries. Clients show it verbatim.
	aiLabel                 = "AI-generated · Daily Post"
	maxThreadsPerDay        = 10
	maxRepliesPerDay        = 60
	autoHideReportThreshold = 3
	maxPageSize             = 50
	defaultPageSize         = 20
)

// WriteByUser is RateLimitPolicy.communityWriteByUser: 10 per 60 s, keyed by user or IP.
var WriteByUser = ratelimit.Policy{Name: "communityWriteByUser", Max: 10, Window: 60 * time.Second}

var (
	errDisabled         = wire.NewError(http.StatusServiceUnavailable, "community_disabled")
	errPostingDisabled  = wire.NewError(http.StatusServiceUnavailable, "community_posting_disabled")
	errProfileRequired  = wire.NewError(http.StatusForbidden, "community_profile_required")
	errTermsRequired403 = wire.NewError(http.StatusForbidden, "terms_required")
	errTermsRequired422 = wire.NewError(http.StatusUnprocessableEntity, "terms_required")
	errSuspended        = wire.NewError(http.StatusForbidden, "posting_suspended")
	errHandleTaken      = wire.NewError(http.StatusConflict, "handle_taken")
	errInvalidHandle    = wire.NewError(http.StatusUnprocessableEntity, "invalid_handle")
	errInvalidName      = wire.NewError(http.StatusUnprocessableEntity, "invalid_display_name")
	errInvalidTitle     = wire.NewError(http.StatusUnprocessableEntity, "invalid_title")
	errInvalidBody      = wire.NewError(http.StatusUnprocessableEntity, "invalid_body")
	errDailyLimit       = wire.NewError(http.StatusTooManyRequests, "daily_limit_reached")
	errNotFound         = wire.NewError(http.StatusNotFound, "post_not_found")
	errInvalidCursor    = wire.NewError(http.StatusBadRequest, "invalid_cursor")
	errCannotReportOwn  = wire.NewError(http.StatusBadRequest, "cannot_report_own_post")
	errCannotBlockSelf  = wire.NewError(http.StatusBadRequest, "cannot_block_self")
	errProfileNotFound  = wire.NewError(http.StatusNotFound, "profile_not_found")
	errAdminRequired    = wire.NewError(http.StatusForbidden, "admin_required")
	errAdminDisabled    = wire.NewError(http.StatusNotFound, "admin disabled")
	errAdminToken       = wire.NewError(http.StatusUnauthorized, "admin token invalid")
)

var reservedHandles = map[string]bool{
	"admin": true, "administrator": true, "ai": true, "daily": true, "dailypost": true, "lumina": true, "luminavault": true,
	"moderator": true, "mod": true, "official": true, "staff": true, "support": true, "system": true, "team": true,
}

var handleRe = regexp.MustCompile(`^[a-z0-9_]{3,20}$`)

// Config holds the switches, read once at boot. Swift compares each lowercased
// value to "true", so anything else is off rather than a boot error.
type Config struct {
	Enabled        bool
	PostingEnabled bool
	AdminToken     string
}

// ParseFlag is CommunityConfig's flag(): the value lowercased, equal to "true".
func ParseFlag(v string) bool { return strings.ToLower(v) == "true" }

type Module struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	cfg       Config
	moderator Moderator
	identify  echo.MiddlewareFunc
	limits    *ratelimit.Store
	now       func() time.Time
}

// New builds the module. identify is auth.Identify and limits the process's
// shared rate-limit store. moderator nil means FilterOnly.
func New(pool *pgxpool.Pool, cfg Config, identify echo.MiddlewareFunc, limits *ratelimit.Store, moderator Moderator) *Module {
	if moderator == nil {
		moderator = FilterOnly{}
	}
	return &Module{pool: pool, q: db.New(pool), cfg: cfg, moderator: moderator, identify: identify, limits: limits, now: time.Now}
}

// SetClock replaces the clock (tests).
func (m *Module) SetClock(now func() time.Time) { m.now = now }

// Register ignores the shared auth middleware: both groups are
// jwtAuthenticator + a rate limit in Swift, with the 401 raised in the handler.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	read := []echo.MiddlewareFunc{m.identify, ratelimit.Middleware(m.limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey)}
	r.GET("/v1/community/me", m.me, read...)
	r.GET("/v1/community/posts", m.listThreads, read...)
	r.GET("/v1/community/posts/:postID", m.getPost, read...)
	r.GET("/v1/community/posts/:postID/replies", m.listReplies, read...)
	r.GET("/v1/community/daily/today", m.todayDaily, read...)
	r.GET("/v1/community/blocks", m.listBlocks, read...)
	r.GET("/v1/community/admin/queue", m.adminQueue, read...)

	write := []echo.MiddlewareFunc{m.identify, ratelimit.Middleware(m.limits, WriteByUser, ratelimit.UserOrIPKey)}
	r.PUT("/v1/community/profile", m.putProfile, write...)
	r.POST("/v1/community/posts", m.createThread, write...)
	r.POST("/v1/community/posts/:postID/replies", m.createReply, write...)
	r.DELETE("/v1/community/posts/:postID", m.deletePost, write...)
	r.POST("/v1/community/posts/:postID/report", m.report, write...)
	r.POST("/v1/community/blocks", m.block, write...)
	r.DELETE("/v1/community/blocks/:profileID", m.unblock, write...)
	r.POST("/v1/community/admin/posts/:postID/decision", m.decide, write...)
	r.POST("/v1/community/admin/profiles/:profileID/suspend", m.suspend, write...)

	// Swift mounts the ops trigger only when the board is on.
	if m.cfg.Enabled {
		r.POST("/v1/admin/community/daily/run", m.dailyRun, m.requireAdminToken)
	}
}

// MARK: - Request bodies, in Swift's declaration order (Bind reports the first
// failing key in field order).

type profileUpsertRequest struct {
	Handle       string `json:"handle"`
	DisplayName  string `json:"displayName"`
	AcceptTerms  bool   `json:"acceptTerms"`
	TermsVersion int    `json:"termsVersion"`
}

type postCreateRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type replyCreateRequest struct {
	Body string `json:"body"`
}

// reportRequest.Reason is a plain string: CommunityReportReason decodes an
// unknown value to .other instead of failing.
type reportRequest struct {
	Reason string  `json:"reason"`
	Note   *string `json:"note"`
}

type blockRequest struct {
	ProfileID wire.UUID `json:"profileId"`
}

// decisionAction is CommunityAdminDecisionAction, whose synthesized decoder
// rejects unknown values.
type decisionAction string

func (a *decisionAction) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch s {
	case "hide", "restore", "remove":
		*a = decisionAction(s)
		return nil
	}
	return &wire.CorruptedError{Message: "Cannot initialize CommunityAdminDecisionAction from invalid String value " + s}
}

type decisionRequest struct {
	Action decisionAction `json:"action"`
	Reason *string        `json:"reason"`
}

// suspendRequest: the spec marks until required, but Swift's is optional (nil lifts).
type suspendRequest struct {
	Until *wire.Time `json:"until"`
}

// MARK: - Member routes

func (m *Module) me(c *echo.Context) error {
	id, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	if !m.cfg.Enabled {
		return wire.JSON(c, http.StatusOK, CommunityMeDTO{TermsVersionRequired: TermsVersion})
	}
	ctx := c.Request().Context()
	user, err := m.q.UserInfo(ctx, id)
	if err != nil {
		return err
	}
	profile, err := m.profile(ctx, id)
	if err != nil {
		return err
	}
	out := CommunityMeDTO{
		Enabled: true, PostingEnabled: m.cfg.PostingEnabled,
		TermsVersionRequired: TermsVersion, IsModerator: user.IsAdmin,
	}
	if profile != nil {
		p := profileDTO(*profile)
		out.Profile = &p
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) putProfile(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	id, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	body, err := wire.Bind[profileUpsertRequest](c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	user, err := m.q.UserInfo(ctx, id)
	if err != nil {
		return err
	}
	handle := strings.ToLower(swiftstr.TrimWhitespacesAndNewlines(body.Handle))
	displayName := swiftstr.TrimWhitespacesAndNewlines(body.DisplayName)
	if !IsValidHandle(handle, user.Username) {
		return errInvalidHandle
	}
	if n := swiftstr.Count(displayName); n < 1 || n > 40 || FilterCheck(displayName) != "" {
		return errInvalidName
	}
	if !body.AcceptTerms || body.TermsVersion != TermsVersion {
		return errTermsRequired422
	}
	taken, err := m.q.HandleTaken(ctx, db.HandleTakenParams{Handle: handle, UserID: id})
	if err != nil {
		return err
	}
	if taken {
		return errHandleTaken
	}
	// Any failure here is the pre-check above losing a race on lower(handle).
	if err := m.q.UpsertProfile(ctx, db.UpsertProfileParams{
		ID: uuid.New(), UserID: id, Handle: handle, DisplayName: displayName,
		TermsVersion: int32(body.TermsVersion), TermsAcceptedAt: m.now(),
	}); err != nil {
		return errHandleTaken
	}
	profile, err := m.profile(ctx, id)
	if err != nil {
		return err
	}
	if profile == nil {
		return wire.NewError(http.StatusInternalServerError, "profile_not_saved")
	}
	return wire.JSON(c, http.StatusOK, profileDTO(*profile))
}

// IsValidHandle: the username is a login identifier (passkey lookup), and a
// public handle equal to it would hand out half of a sign-in.
func IsValidHandle(handle, username string) bool {
	return handleRe.MatchString(handle) && !reservedHandles[handle] && handle != strings.ToLower(username)
}

// viewer is Swift's Viewer: who is reading. profile is nil until the member
// sets one up; they can read, but not post, report or block.
type viewer struct {
	userID  uuid.UUID
	profile *uuid.UUID
}

func (m *Module) viewer(c *echo.Context) (viewer, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return viewer{}, wire.ErrUnauthenticated
	}
	profile, err := m.profile(c.Request().Context(), id)
	if err != nil {
		return viewer{}, err
	}
	v := viewer{userID: id}
	if profile != nil {
		v.profile = &profile.ID
	}
	return v, nil
}

func (m *Module) listThreads(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	v, err := m.viewer(c)
	if err != nil {
		return err
	}
	size := pageSize(limitParam(c))
	args := db.ListThreadsParams{ViewerProfile: v.profile, RowLimit: int32(size + 1)} //nolint:gosec // size is at most maxPageSize
	if raw, ok := queryParam(c, "cursor"); ok {
		micros, id, err := DecodeCursor(raw)
		if err != nil {
			return err
		}
		args.CursorUs, args.CursorID = &micros, &id
	}
	rows, err := m.q.ListThreads(c.Request().Context(), args)
	if err != nil {
		return err
	}
	posts := make([]postRow, len(rows))
	for i, r := range rows {
		posts[i] = postRow(r)
	}
	return wire.JSON(c, http.StatusOK, page(posts, size, v, func(r postRow) int64 { return r.ActivityUs }))
}

func (m *Module) getPost(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	v, err := m.viewer(c)
	if err != nil {
		return err
	}
	id, err := uuidParam(c, "postID")
	if err != nil {
		return err
	}
	post, err := m.post(c.Request().Context(), v, id)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, post)
}

func (m *Module) listReplies(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	v, err := m.viewer(c)
	if err != nil {
		return err
	}
	parent, err := uuidParam(c, "postID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if _, err := m.post(ctx, v, parent); err != nil {
		return err
	}
	size := pageSize(limitParam(c))
	args := db.ListRepliesParams{ParentID: &parent, ViewerProfile: v.profile, RowLimit: int32(size + 1)} //nolint:gosec // size is at most maxPageSize
	if raw, ok := queryParam(c, "cursor"); ok {
		micros, id, err := DecodeCursor(raw)
		if err != nil {
			return err
		}
		args.CursorUs, args.CursorID = &micros, &id
	}
	rows, err := m.q.ListReplies(ctx, args)
	if err != nil {
		return err
	}
	posts := make([]postRow, len(rows))
	for i, r := range rows {
		posts[i] = postRow(r)
	}
	return wire.JSON(c, http.StatusOK, page(posts, size, v, func(r postRow) int64 { return r.CreatedUs }))
}

func (m *Module) todayDaily(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	v, err := m.viewer(c)
	if err != nil {
		return err
	}
	row, err := m.q.TodayDaily(c.Request().Context(), db.TodayDailyParams{Day: utcDay(m.now()), ViewerProfile: v.profile})
	if errors.Is(err, pgx.ErrNoRows) {
		return c.NoContent(http.StatusNoContent)
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, postDTO(postRow(row), v))
}

// MARK: - Writing

func (m *Module) createThread(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	body, err := wire.Bind[postCreateRequest](c)
	if err != nil {
		return err
	}
	id, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	title := swiftstr.TrimWhitespacesAndNewlines(body.Title)
	text := swiftstr.TrimWhitespacesAndNewlines(body.Body)
	if n := swiftstr.Count(title); n < 3 || n > 120 {
		return errInvalidTitle
	}
	if err := validateBody(text); err != nil {
		return err
	}
	author, err := m.requirePoster(ctx, id)
	if err != nil {
		return err
	}
	if err := m.enforceDailyCap(ctx, author.ID, false); err != nil {
		return err
	}
	status, err := m.moderate(ctx, &title, text)
	if err != nil {
		return err
	}
	postID := uuid.New()
	now := m.now()
	if err := m.q.InsertPost(ctx, insertArgs(postID, nil, author.ID, &title, text, status, now)); err != nil {
		return err
	}
	post, err := m.post(ctx, viewer{userID: id, profile: &author.ID}, postID)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusCreated, post)
}

func (m *Module) createReply(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	body, err := wire.Bind[replyCreateRequest](c)
	if err != nil {
		return err
	}
	id, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	parentID, err := uuidParam(c, "postID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	text := swiftstr.TrimWhitespacesAndNewlines(body.Body)
	if err := validateBody(text); err != nil {
		return err
	}
	author, err := m.requirePoster(ctx, id)
	if err != nil {
		return err
	}
	v := viewer{userID: id, profile: &author.ID}
	parent, err := m.post(ctx, v, parentID)
	if err != nil {
		return err
	}
	// One level of replies, and only under a thread that is on the board.
	if parent.ParentId != nil || parent.Status != CommunityPostDTOStatusVisible {
		return errNotFound
	}
	if err := m.enforceDailyCap(ctx, author.ID, true); err != nil {
		return err
	}
	status, err := m.moderate(ctx, nil, text)
	if err != nil {
		return err
	}
	postID := uuid.New()
	now := m.now()
	if err := m.q.InsertPost(ctx, insertArgs(postID, &parentID, author.ID, nil, text, status, now)); err != nil {
		return err
	}
	if status == "visible" {
		if err := m.refreshThread(ctx, parentID, &now); err != nil {
			return err
		}
	}
	post, err := m.post(ctx, v, postID)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusCreated, post)
}

func insertArgs(id uuid.UUID, parent *uuid.UUID, author uuid.UUID, title *string, body, status string, now time.Time) db.InsertPostParams {
	args := db.InsertPostParams{
		ID: id, ParentID: parent, AuthorProfileID: &author, Title: title, Body: body, Status: status, At: now,
	}
	if status == "visible" {
		args.ModeratedAt = &now
	}
	return args
}

// deletePost is an author-only soft delete. The row stays so reports keep their target.
func (m *Module) deletePost(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	id, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	postID, err := uuidParam(c, "postID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	profile, err := m.profile(ctx, id)
	if err != nil {
		return err
	}
	if profile == nil {
		return errNotFound
	}
	parent, err := m.q.SoftDeletePost(ctx, db.SoftDeletePostParams{ID: postID, AuthorProfileID: &profile.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if parent != nil {
		if err := m.refreshThread(ctx, *parent, nil); err != nil {
			return err
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// report files a report. The third distinct open report takes the post off the
// board until a moderator rules on it.
func (m *Module) report(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	body, err := wire.Bind[reportRequest](c)
	if err != nil {
		return err
	}
	id, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	postID, err := uuidParam(c, "postID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	reporter, err := m.requireProfile(ctx, id)
	if err != nil {
		return err
	}
	target, err := m.post(ctx, viewer{userID: id, profile: &reporter.ID}, postID)
	if err != nil {
		return err
	}
	if target.IsOwn {
		return errCannotReportOwn
	}
	var note *string
	if body.Note != nil {
		if n := swiftstr.Prefix(swiftstr.TrimWhitespacesAndNewlines(*body.Note), 500); n != "" {
			note = &n
		}
	}
	if err := m.q.InsertReport(ctx, db.InsertReportParams{
		ID: uuid.New(), PostID: postID, ReporterProfileID: reporter.ID, Reason: reportReason(body.Reason), Note: note,
	}); err != nil {
		return err
	}
	parent, err := m.q.AutoHide(ctx, db.AutoHideParams{PostID: postID, Threshold: autoHideReportThreshold})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	case parent != nil:
		if err := m.refreshThread(ctx, *parent, nil); err != nil {
			return err
		}
	}
	return c.NoContent(http.StatusAccepted)
}

func reportReason(s string) string {
	switch s {
	case "spam", "harassment", "hate", "sexual", "violence", "self_harm", "misinformation", "other":
		return s
	}
	return "other"
}

// MARK: - Blocks

func (m *Module) listBlocks(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	id, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	me, err := m.profile(ctx, id)
	if err != nil {
		return err
	}
	items := []CommunityBlockedProfileDTO{}
	if me != nil {
		rows, err := m.q.ListBlocks(ctx, me.ID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			items = append(items, CommunityBlockedProfileDTO{
				ProfileId: wire.UUID{UUID: r.BlockedProfileID}, Handle: r.Handle,
				DisplayName: r.DisplayName, BlockedAt: wire.Time{Time: r.CreatedAt},
			})
		}
	}
	return wire.JSON(c, http.StatusOK, CommunityBlockListDTO{Items: items})
}

func (m *Module) block(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	body, err := wire.Bind[blockRequest](c)
	if err != nil {
		return err
	}
	id, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	ctx := c.Request().Context()
	me, err := m.requireProfile(ctx, id)
	if err != nil {
		return err
	}
	if me.ID == body.ProfileID.UUID {
		return errCannotBlockSelf
	}
	exists, err := m.q.ProfileExists(ctx, body.ProfileID.UUID)
	if err != nil {
		return err
	}
	if !exists {
		return errProfileNotFound
	}
	if err := m.q.InsertBlock(ctx, db.InsertBlockParams{BlockerProfileID: me.ID, BlockedProfileID: body.ProfileID.UUID}); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) unblock(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	id, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	target, err := uuidParam(c, "profileID")
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	me, err := m.profile(ctx, id)
	if err != nil {
		return err
	}
	if me != nil {
		if err := m.q.DeleteBlock(ctx, db.DeleteBlockParams{BlockerProfileID: me.ID, BlockedProfileID: target}); err != nil {
			return err
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// MARK: - Moderators (users.is_admin)

func (m *Module) requireModerator(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, wire.ErrUnauthenticated
	}
	user, err := m.q.UserInfo(c.Request().Context(), id)
	if err != nil {
		return uuid.Nil, err
	}
	if !user.IsAdmin {
		return uuid.Nil, errAdminRequired
	}
	return id, nil
}

func (m *Module) adminQueue(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	if _, err := m.requireModerator(c); err != nil {
		return err
	}
	filter := "all"
	if raw, ok := queryParam(c, "status"); ok {
		switch raw {
		case "open", "pending", "hidden":
			filter = raw
		}
	}
	rows, err := m.q.AdminQueue(c.Request().Context(), filter)
	if err != nil {
		return err
	}
	moderator := viewer{userID: uuid.New()}
	items := make([]CommunityAdminQueueItemDTO, len(rows))
	for i, r := range rows {
		item := CommunityAdminQueueItemDTO{
			Post: postDTO(postRow{
				ID: r.ID, ParentID: r.ParentID, Title: r.Title, Body: r.Body, AuthorKind: r.AuthorKind,
				AuthorProfileID: r.AuthorProfileID, Handle: r.Handle, DisplayName: r.DisplayName, Status: r.Status,
				ReplyCount: r.ReplyCount, ReactionCount: r.ReactionCount, CreatedAt: r.CreatedAt,
				LastActivityAt: r.LastActivityAt, IsDaily: r.IsDaily,
			}, moderator),
			ModerationCategory: r.ModerationCategory,
			HiddenReason:       r.HiddenReason,
			OpenReports:        int(r.OpenReports),
			ReportReasons:      []CommunityAdminQueueItemDTOReportReasons{},
		}
		for _, reason := range r.ReportReasons {
			item.ReportReasons = append(item.ReportReasons, CommunityAdminQueueItemDTOReportReasons(reason))
		}
		if at, ok := r.LatestReportAt.(time.Time); ok {
			item.LatestReportAt = &wire.Time{Time: at}
		}
		items[i] = item
	}
	return wire.JSON(c, http.StatusOK, CommunityAdminQueueDTO{Items: items})
}

func (m *Module) decide(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	admin, err := m.requireModerator(c)
	if err != nil {
		return err
	}
	req, err := wire.Bind[decisionRequest](c)
	if err != nil {
		return err
	}
	postID, err := uuidParam(c, "postID")
	if err != nil {
		return err
	}
	args := db.DecideParams{ID: postID}
	var reportStatus string
	switch req.Action {
	case "hide":
		args.Status, reportStatus = "hidden", "actioned"
	case "restore":
		args.Status, reportStatus = "visible", "dismissed"
	case "remove":
		removed := "[removed by a moderator]"
		args.Status, reportStatus, args.Body = "deleted", "actioned", &removed
	}
	if req.Action != "restore" {
		reason := "moderator"
		if req.Reason != nil {
			reason = swiftstr.Prefix(*req.Reason, 500)
		}
		args.HiddenReason = &reason
	}
	ctx := c.Request().Context()
	parent, err := m.q.Decide(ctx, args)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if err := m.q.ResolveReports(ctx, db.ResolveReportsParams{Status: reportStatus, ResolvedBy: &admin, PostID: postID}); err != nil {
		return err
	}
	if parent != nil {
		if err := m.refreshThread(ctx, *parent, nil); err != nil {
			return err
		}
	}
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) suspend(c *echo.Context) error {
	if !m.cfg.Enabled {
		return errDisabled
	}
	if _, err := m.requireModerator(c); err != nil {
		return err
	}
	req, err := wire.Bind[suspendRequest](c)
	if err != nil {
		return err
	}
	profileID, err := uuidParam(c, "profileID")
	if err != nil {
		return err
	}
	var until *time.Time
	if req.Until != nil {
		until = &req.Until.Time
	}
	_, err = m.q.Suspend(c.Request().Context(), db.SuspendParams{Until: until, ID: profileID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errProfileNotFound
	}
	if err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// MARK: - Internals

// postRow is the one shape a post leaves the database in.
type postRow = db.GetPostRow

func (m *Module) post(ctx context.Context, v viewer, id uuid.UUID) (CommunityPostDTO, error) {
	row, err := m.q.GetPost(ctx, db.GetPostParams{ID: id, ViewerProfile: v.profile})
	if errors.Is(err, pgx.ErrNoRows) {
		return CommunityPostDTO{}, errNotFound
	}
	if err != nil {
		return CommunityPostDTO{}, err
	}
	return postDTO(row, v), nil
}

func postDTO(r postRow, v viewer) CommunityPostDTO {
	kind := CommunityAuthorDTOKind(r.AuthorKind)
	author := CommunityAuthorDTO{Kind: kind, Handle: r.Handle, DisplayName: r.DisplayName}
	if r.AuthorProfileID != nil {
		author.ProfileId = &wire.UUID{UUID: *r.AuthorProfileID}
	}
	out := CommunityPostDTO{
		Id: wire.UUID{UUID: r.ID}, Title: r.Title, Body: r.Body, Author: author, IsDailyPost: r.IsDaily,
		Status: CommunityPostDTOStatus(r.Status), ReplyCount: int(r.ReplyCount), ReactionCount: int(r.ReactionCount),
		IsOwn:     v.profile != nil && r.AuthorProfileID != nil && *r.AuthorProfileID == *v.profile,
		CreatedAt: wire.Time{Time: r.CreatedAt}, LastActivityAt: wire.Time{Time: r.LastActivityAt},
	}
	if r.ParentID != nil {
		out.ParentId = &wire.UUID{UUID: *r.ParentID}
	}
	if kind == CommunityAuthorDTOKindAi {
		label := aiLabel
		out.AiLabel = &label
	}
	return out
}

func profileDTO(r db.ProfileByUserRow) CommunityProfileDTO {
	out := CommunityProfileDTO{
		Id: wire.UUID{UUID: r.ID}, Handle: r.Handle, DisplayName: r.DisplayName,
		TermsVersion: int(r.TermsVersion), TermsAcceptedAt: wire.Time{Time: r.TermsAcceptedAt},
	}
	if r.PostingSuspendedUntil != nil {
		out.PostingSuspendedUntil = &wire.Time{Time: *r.PostingSuspendedUntil}
	}
	return out
}

func page(rows []postRow, size int, v viewer, micros func(postRow) int64) CommunityPostPageDTO {
	items := rows
	var next *string
	if len(rows) > size {
		items = rows[:size]
		last := items[len(items)-1]
		c := EncodeCursor(micros(last), last.ID)
		next = &c
	}
	out := CommunityPostPageDTO{Items: make([]CommunityPostDTO, len(items)), NextCursor: next}
	for i, r := range items {
		out.Items[i] = postDTO(r, v)
	}
	return out
}

func (m *Module) profile(ctx context.Context, userID uuid.UUID) (*db.ProfileByUserRow, error) {
	row, err := m.q.ProfileByUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // no profile yet is a normal answer
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (m *Module) requireProfile(ctx context.Context, userID uuid.UUID) (*db.ProfileByUserRow, error) {
	profile, err := m.profile(ctx, userID)
	if err != nil {
		return nil, err
	}
	if profile == nil {
		return nil, errProfileRequired
	}
	return profile, nil
}

// requirePoster is a member who may post right now: posting switched on,
// profile, current terms, not suspended.
func (m *Module) requirePoster(ctx context.Context, userID uuid.UUID) (*db.ProfileByUserRow, error) {
	if !m.cfg.PostingEnabled {
		return nil, errPostingDisabled
	}
	profile, err := m.requireProfile(ctx, userID)
	if err != nil {
		return nil, err
	}
	if profile.TermsVersion < TermsVersion {
		return nil, errTermsRequired403
	}
	if until := profile.PostingSuspendedUntil; until != nil && until.After(m.now()) {
		return nil, errSuspended
	}
	return profile, nil
}

func (m *Module) enforceDailyCap(ctx context.Context, profileID uuid.UUID, replies bool) error {
	n, err := m.q.CountRecentPosts(ctx, db.CountRecentPostsParams{AuthorProfileID: &profileID, Replies: replies})
	if err != nil {
		return err
	}
	limit := maxThreadsPerDay
	if replies {
		limit = maxRepliesPerDay
	}
	if int(n) >= limit {
		return errDailyLimit
	}
	return nil
}

// moderate returns "visible" or "pending"; a rejection is a 422.
func (m *Module) moderate(ctx context.Context, title *string, body string) (string, error) {
	v := m.moderator.Review(ctx, title, body)
	switch v.Kind {
	case Allowed:
		return "visible", nil
	case Rejected:
		return "", wire.NewError(http.StatusUnprocessableEntity, "moderation_rejected:"+v.Category)
	default:
		return "pending", nil
	}
}

// refreshThread recounts a thread's visible replies; touchedAt also bumps it to
// the top of the board.
func (m *Module) refreshThread(ctx context.Context, id uuid.UUID, touchedAt *time.Time) error {
	touched := pgtype.Timestamptz{}
	if touchedAt != nil {
		touched = pgtype.Timestamptz{Time: *touchedAt, Valid: true}
	}
	return m.q.RefreshThread(ctx, db.RefreshThreadParams{ID: &id, TouchedAt: touched})
}

func validateBody(body string) error {
	if n := swiftstr.Count(body); n < 1 || n > 4000 {
		return errInvalidBody
	}
	return nil
}

func pageSize(limit int) int { return min(max(limit, 1), maxPageSize) }

func limitParam(c *echo.Context) int {
	if raw, ok := queryParam(c, "limit"); ok {
		if n, err := strconv.Atoi(raw); err == nil {
			return n
		}
	}
	return defaultPageSize
}

// queryParam tells a missing parameter from an empty one: Swift treats
// ?cursor= as a cursor (and a bad one, a 400).
func queryParam(c *echo.Context, name string) (string, bool) {
	vals, ok := c.QueryParams()[name]
	if !ok || len(vals) == 0 {
		return "", false
	}
	return vals[0], true
}

func uuidParam(c *echo.Context, name string) (uuid.UUID, error) {
	raw := c.Param(name)
	id, err := ids.Parse(raw)
	if err != nil || len(raw) != 36 {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "invalid_"+name)
	}
	return id, nil
}

// EncodeCursor is base64url("<epoch microseconds>|<UUID>"). Microseconds keep
// the comparison exact against Postgres timestamps.
func EncodeCursor(micros int64, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(micros, 10) + "|" + ids.Upper(id)))
}

// DecodeCursor reverses EncodeCursor; anything else is a 400.
func DecodeCursor(cursor string) (int64, uuid.UUID, error) {
	b64 := strings.NewReplacer("-", "+", "_", "/").Replace(cursor)
	for len(b64)%4 != 0 {
		b64 += "="
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || !utf8.Valid(raw) {
		return 0, uuid.Nil, errInvalidCursor
	}
	parts := strings.FieldsFunc(string(raw), func(r rune) bool { return r == '|' })
	if len(parts) != 2 {
		return 0, uuid.Nil, errInvalidCursor
	}
	micros, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, uuid.Nil, errInvalidCursor
	}
	id, err := ids.Parse(parts[1])
	if err != nil || len(parts[1]) != 36 {
		return 0, uuid.Nil, errInvalidCursor
	}
	return micros, id, nil
}

func utcDay(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// MARK: - Admin trigger

// requireAdminToken is AdminTokenMiddleware: X-Admin-Token against ADMIN_TOKEN.
// An empty token rejects everything, so a misconfigured prod exposes nothing.
func (m *Module) requireAdminToken(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if m.cfg.AdminToken == "" {
			return errAdminDisabled
		}
		got := c.Request().Header.Get("X-Admin-Token")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(m.cfg.AdminToken)) != 1 {
			return errAdminToken
		}
		return next(c)
	}
}

const (
	dailyMaxAttempts = 3
	dailyStaleClaim  = 30 * time.Minute
)

// errNoGeneration stands in for DailyPostWorker.generate: writing the post needs
// LLM routing (wave 5), so every claimed attempt fails here. Production keeps
// /v1/admin/community on Swift until then. See the PR's gap note.
var errNoGeneration = errors.New("daily post generation is not ported to Go (LLM routing, wave 5)")

// dailyRun is DailyPostWorker.publishDaily without the post: it claims today,
// records the failed attempt, and reports the day's row.
func (m *Module) dailyRun(c *echo.Context) error {
	ctx := c.Request().Context()
	now := m.now()
	day := utcDay(now)
	claimed, err := m.claim(ctx, day, now)
	if err != nil {
		return err
	}
	if claimed {
		if err := m.q.MarkDayFailed(ctx, db.MarkDayFailedParams{
			MaxAttempts: dailyMaxAttempts, LastError: new(errNoGeneration.Error()), Day: day,
		}); err != nil {
			return err
		}
	}
	out := AdminCommunityDailyRunResult{Day: day, Status: None}
	row, err := m.q.DayOutcome(ctx, day)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	default:
		out.Status = AdminCommunityDailyRunResultStatus(row.Status)
		out.TopicKey = row.TopicKey
		if row.PostID != nil {
			out.PostId = &wire.UUID{UUID: *row.PostID}
		}
		if row.SourceKind != nil {
			kind := AdminCommunityDailyRunResultSourceKind(*row.SourceKind)
			out.SourceKind = &kind
		}
	}
	return wire.JSON(c, http.StatusOK, out)
}

// claim is true when this caller owns today's attempt. A failed attempt, or a
// claim whose pod died mid-run, may be retried.
func (m *Module) claim(ctx context.Context, day string, now time.Time) (bool, error) {
	_, err := m.q.ClaimDay(ctx, db.ClaimDayParams{Day: day, At: &now})
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	stale := now.Add(-dailyStaleClaim)
	_, err = m.q.ReclaimDay(ctx, db.ReclaimDayParams{At: &now, Day: day, MaxAttempts: dailyMaxAttempts, StaleBefore: &stale})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
