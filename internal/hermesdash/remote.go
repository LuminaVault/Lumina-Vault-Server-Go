package hermesdash

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
)

// Remote is RemoteHermesTransport: the user's own Hermes. The dashboard is
// optional (a public bind offers cookie/PKCE only); the gateway api_server
// serves skills, cron rows and sessions, so the reads that matter work with a
// gateway alone. Filesystem access has no gateway equivalent.
type Remote struct {
	GatewayBaseURL    *url.URL
	GatewayAuthHeader *string
	// Gateway is set whenever the tenant has a gateway resolution.
	Gateway   *Gateway
	Dashboard *Dashboard
	// SkillsHTTP is HermesSkillsClient's HTTP; nil uses the default client.
	SkillsHTTP Doer
}

func (r *Remote) Kind() TransportKind { return KindRemote }

func (r *Remote) requireDashboard() (*Dashboard, error) {
	if r.Dashboard == nil {
		return nil, ErrAuthModeUnsupported
	}
	return r.Dashboard, nil
}

func (r *Remote) requireGateway() (*Gateway, error) {
	if r.Gateway == nil {
		return nil, Unsupported("gateway")
	}
	return r.Gateway, nil
}

func transportErr(err error) (*Error, bool) {
	var te *Error
	ok := errors.As(err, &te)
	return te, ok
}

func (r *Remote) Status(ctx context.Context) (Status, error) {
	if r.Dashboard != nil {
		return r.Dashboard.Status(ctx)
	}
	required := true
	return Status{Reachable: r.GatewayBaseURL != nil, AuthRequired: &required}, nil
}

func (r *Remote) ListSkills(ctx context.Context) ([]Skill, error) {
	var skills []Skill
	var err error
	if r.Dashboard == nil {
		skills, err = r.gatewaySkills(ctx)
	} else {
		skills, err = r.Dashboard.ListSkills(ctx)
	}
	if err == nil {
		return skills, nil
	}
	if _, ok := transportErr(err); !ok || r.GatewayBaseURL == nil {
		return nil, err
	}
	slog.DebugContext(ctx, "dashboard skills unavailable; falling back to gateway", "error", err.Error())
	fallback, ferr := r.gatewaySkills(ctx)
	if ferr != nil || len(fallback) == 0 {
		return nil, err
	}
	return fallback, nil
}

// gatewaySkills reads /v1/skills: each entry's titleized name and summary.
func (r *Remote) gatewaySkills(ctx context.Context) ([]Skill, error) {
	if r.GatewayBaseURL == nil {
		return nil, Unsupported("skills")
	}
	entries := InstalledSkills(ctx, r.SkillsHTTP, r.GatewayBaseURL, r.GatewayAuthHeader)
	skills := make([]Skill, 0, len(entries))
	for _, e := range entries {
		skills = append(skills, Skill{Name: e.Name, Description: e.Summary, Enabled: true, Source: "custom"})
	}
	return skills, nil
}

func (r *Remote) SkillContent(ctx context.Context, name string) (string, error) {
	d, err := r.requireDashboard()
	if err != nil {
		return "", err
	}
	return d.SkillContent(ctx, name)
}

func (r *Remote) ToggleSkill(ctx context.Context, name string, enabled bool) error {
	d, err := r.requireDashboard()
	if err != nil {
		return err
	}
	return d.ToggleSkill(ctx, name, enabled)
}

func (r *Remote) CreateSkill(ctx context.Context, name, content string) error {
	d, err := r.requireDashboard()
	if err != nil {
		return err
	}
	return d.CreateSkill(ctx, name, content)
}

func (r *Remote) ListJobs(ctx context.Context) ([]Job, error) {
	jobs, err := func() ([]Job, error) {
		if r.Dashboard == nil {
			g, err := r.requireGateway()
			if err != nil {
				return nil, err
			}
			return g.ListJobs(ctx)
		}
		return r.Dashboard.ListJobs(ctx)
	}()
	if err == nil {
		return jobs, nil
	}
	if _, ok := transportErr(err); !ok || r.Gateway == nil {
		return nil, err
	}
	slog.DebugContext(ctx, "dashboard jobs unavailable; falling back to gateway", "error", err.Error())
	return r.Gateway.ListJobs(ctx)
}

// withJobWriteFallback: dashboard first when linked, the gateway otherwise; a
// transport error falls through to the gateway (again, when it was the gateway
// that failed, as in Swift).
func (r *Remote) withJobWriteFallback(ctx context.Context, op string, viaDashboard func(*Dashboard) (Job, error), viaGateway func(*Gateway) (Job, error)) (Job, error) {
	job, err := func() (Job, error) {
		if r.Dashboard != nil {
			return viaDashboard(r.Dashboard)
		}
		g, err := r.requireGateway()
		if err != nil {
			return Job{}, err
		}
		return viaGateway(g)
	}()
	if err == nil {
		return job, nil
	}
	if _, ok := transportErr(err); !ok || r.Gateway == nil {
		return Job{}, err
	}
	slog.DebugContext(ctx, "dashboard "+op+" unavailable; falling back to gateway", "error", err.Error())
	return viaGateway(r.Gateway)
}

func (r *Remote) CreateJob(ctx context.Context, spec JobSpec) (Job, error) {
	return r.withJobWriteFallback(ctx, "createJob",
		func(d *Dashboard) (Job, error) { return d.CreateJob(ctx, spec) },
		func(g *Gateway) (Job, error) { return g.CreateJob(ctx, spec) })
}

func (r *Remote) UpdateJob(ctx context.Context, id string, updates JobUpdate) (Job, error) {
	return r.withJobWriteFallback(ctx, "updateJob",
		func(d *Dashboard) (Job, error) { return d.UpdateJob(ctx, id, updates) },
		func(g *Gateway) (Job, error) { return g.UpdateJob(ctx, id, updates) })
}

func (r *Remote) PauseJob(ctx context.Context, id string) (Job, error) {
	return r.withJobWriteFallback(ctx, "pauseJob",
		func(d *Dashboard) (Job, error) { return d.PauseJob(ctx, id) },
		func(g *Gateway) (Job, error) { return g.PauseJob(ctx, id) })
}

func (r *Remote) ResumeJob(ctx context.Context, id string) (Job, error) {
	return r.withJobWriteFallback(ctx, "resumeJob",
		func(d *Dashboard) (Job, error) { return d.ResumeJob(ctx, id) },
		func(g *Gateway) (Job, error) { return g.ResumeJob(ctx, id) })
}

func (r *Remote) TriggerJob(ctx context.Context, id string) (Job, error) {
	return r.withJobWriteFallback(ctx, "triggerJob",
		func(d *Dashboard) (Job, error) { return d.TriggerJob(ctx, id) },
		func(g *Gateway) (Job, error) { return g.TriggerJob(ctx, id) })
}

func (r *Remote) DeleteJob(ctx context.Context, id string) error {
	err := func() error {
		if r.Dashboard != nil {
			return r.Dashboard.DeleteJob(ctx, id)
		}
		g, err := r.requireGateway()
		if err != nil {
			return err
		}
		return g.DeleteJob(ctx, id)
	}()
	if err == nil {
		return nil
	}
	if _, ok := transportErr(err); !ok || r.Gateway == nil {
		return err
	}
	slog.DebugContext(ctx, "dashboard deleteJob unavailable; falling back to gateway", "error", err.Error())
	return r.Gateway.DeleteJob(ctx, id)
}

func (r *Remote) JobRuns(ctx context.Context, jobID string, limit int) ([]JobRun, error) {
	d, err := r.requireDashboard()
	if err != nil {
		return nil, err
	}
	return d.JobRuns(ctx, jobID, limit)
}

func (r *Remote) JobRunOutput(ctx context.Context, jobID, runKey string) (*string, error) {
	d, err := r.requireDashboard()
	if err != nil {
		return nil, err
	}
	return d.JobRunOutput(ctx, jobID, runKey)
}

func (r *Remote) ListFiles(ctx context.Context, path string) ([]FileEntry, error) {
	d, err := r.requireDashboard()
	if err != nil {
		return nil, err
	}
	return d.ListFiles(ctx, path)
}

func (r *Remote) ReadText(ctx context.Context, path string) (string, error) {
	d, err := r.requireDashboard()
	if err != nil {
		return "", err
	}
	return d.ReadText(ctx, path)
}

func (r *Remote) WriteText(ctx context.Context, path, content string) error {
	d, err := r.requireDashboard()
	if err != nil {
		return err
	}
	return d.WriteText(ctx, path, content)
}

func (r *Remote) Mkdir(ctx context.Context, path string) error {
	d, err := r.requireDashboard()
	if err != nil {
		return err
	}
	return d.Mkdir(ctx, path)
}

func (r *Remote) ListSessions(ctx context.Context, offset, limit int) (SessionPage, error) {
	page, err := func() (SessionPage, error) {
		if r.Dashboard == nil {
			g, err := r.requireGateway()
			if err != nil {
				return SessionPage{}, err
			}
			return g.ListSessions(ctx, offset, limit)
		}
		return r.Dashboard.ListSessions(ctx, offset, limit)
	}()
	if err == nil {
		return page, nil
	}
	if _, ok := transportErr(err); !ok || r.Gateway == nil {
		return SessionPage{}, err
	}
	return r.Gateway.ListSessions(ctx, offset, limit)
}

func (r *Remote) SessionMessages(ctx context.Context, id string) ([]Message, error) {
	messages, err := func() ([]Message, error) {
		if r.Dashboard == nil {
			g, err := r.requireGateway()
			if err != nil {
				return nil, err
			}
			return g.SessionMessages(ctx, id)
		}
		return r.Dashboard.SessionMessages(ctx, id)
	}()
	if err == nil {
		return messages, nil
	}
	if _, ok := transportErr(err); !ok || r.Gateway == nil {
		return nil, err
	}
	return r.Gateway.SessionMessages(ctx, id)
}
