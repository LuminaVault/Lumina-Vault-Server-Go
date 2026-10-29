CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS "uuid-ossp" WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;
CREATE TABLE public.achievement_progress (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    achievement_key text NOT NULL,
    progress_count bigint DEFAULT 0 NOT NULL,
    unlocked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.agent_connections (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    client_kind text NOT NULL,
    token_hash bytea NOT NULL,
    token_prefix text NOT NULL,
    created_at timestamp with time zone,
    last_used_at timestamp with time zone,
    revoked_at timestamp with time zone,
    allow_personal_data boolean DEFAULT false NOT NULL,
    access text DEFAULT 'read'::text NOT NULL,
    CONSTRAINT agent_connections_access_check CHECK ((access = ANY (ARRAY['read'::text, 'read_write'::text])))
);
CREATE TABLE public.agent_room_members (
    id uuid NOT NULL,
    room_id uuid NOT NULL,
    instance_id text NOT NULL,
    profile text,
    handle text NOT NULL,
    display_name text NOT NULL,
    respond_mode text NOT NULL,
    created_at timestamp with time zone
);
CREATE TABLE public.agent_room_messages (
    id uuid NOT NULL,
    room_id uuid NOT NULL,
    author_kind text NOT NULL,
    member_id uuid,
    body text NOT NULL,
    tokens bigint,
    created_at timestamp with time zone
);
CREATE TABLE public.agent_rooms (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    title text NOT NULL,
    token_budget bigint DEFAULT 200000 NOT NULL,
    spent_tokens bigint DEFAULT 0 NOT NULL,
    stop_requested_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.agent_turn_traces (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    conversation_message_id uuid,
    provider text NOT NULL,
    model text NOT NULL,
    tool_calls jsonb,
    failover_count integer DEFAULT 0 NOT NULL,
    tokens_in bigint DEFAULT 0 NOT NULL,
    tokens_out bigint DEFAULT 0 NOT NULL,
    estimated_cost_usd_micros bigint DEFAULT 0 NOT NULL,
    latency_ms bigint DEFAULT 0 NOT NULL,
    credential_mode text,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.analytics_daily_rollups (
    day date NOT NULL,
    vault_id uuid NOT NULL,
    actor_user_id uuid NOT NULL,
    metric text NOT NULL,
    dimension_key text DEFAULT ''::text NOT NULL,
    value bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.analytics_events (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    vault_id uuid NOT NULL,
    actor_user_id uuid NOT NULL,
    event_name text NOT NULL,
    source text NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    dimensions jsonb DEFAULT '{}'::jsonb NOT NULL,
    idempotency_key text,
    CONSTRAINT analytics_events_source_check CHECK ((source = ANY (ARRAY['server'::text, 'ios'::text, 'web'::text])))
);
CREATE TABLE public.analytics_recommendation_states (
    vault_id uuid NOT NULL,
    user_id uuid NOT NULL,
    recommendation_id text NOT NULL,
    dismissed_at timestamp with time zone,
    snoozed_until timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.apns_category_prefs (
    tenant_id uuid NOT NULL,
    chat_enabled boolean DEFAULT true NOT NULL,
    nudge_enabled boolean DEFAULT true NOT NULL,
    digest_enabled boolean DEFAULT true NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    approval_enabled boolean DEFAULT true NOT NULL,
    run_completed_enabled boolean DEFAULT true NOT NULL,
    meeting_brief_enabled boolean DEFAULT false NOT NULL
);
CREATE TABLE public.apple_consent (
    tenant_id uuid NOT NULL,
    domain text NOT NULL,
    allowed boolean DEFAULT false NOT NULL,
    allow_writes boolean DEFAULT false NOT NULL,
    last_sync_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.apple_reminders (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    external_id text NOT NULL,
    title text NOT NULL,
    notes text,
    due_at timestamp with time zone,
    completed boolean DEFAULT false NOT NULL,
    completed_at timestamp with time zone,
    list_name text,
    priority bigint,
    remote_updated_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.billing_event_logs (
    id uuid NOT NULL,
    event_id text NOT NULL,
    event_type text NOT NULL,
    user_id uuid,
    processed_at timestamp with time zone
);
CREATE TABLE public.brief_feedback (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    brief_id uuid NOT NULL,
    card_id text NOT NULL,
    card_kind text NOT NULL,
    action text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT brief_feedback_action_check CHECK ((action = ANY (ARRAY['up'::text, 'down'::text, 'dismiss'::text])))
);
CREATE TABLE public.briefs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    date date NOT NULL,
    cards jsonb DEFAULT '[]'::jsonb NOT NULL,
    end_note text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.calendar_accounts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    provider text NOT NULL,
    account_email text,
    scope text NOT NULL,
    refresh_ciphertext bytea,
    refresh_nonce bytea,
    access_ciphertext bytea,
    access_nonce bytea,
    access_expires_at timestamp with time zone,
    sync_token text,
    last_synced_at timestamp with time zone,
    window_start timestamp with time zone,
    window_end timestamp with time zone,
    status text DEFAULT 'connected'::text NOT NULL,
    last_failure_at timestamp with time zone,
    last_failure_code text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.calendar_events (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    source text NOT NULL,
    external_id text NOT NULL,
    calendar_id text,
    title text NOT NULL,
    notes text,
    location text,
    starts_at timestamp with time zone NOT NULL,
    ends_at timestamp with time zone NOT NULL,
    all_day boolean DEFAULT false NOT NULL,
    status text DEFAULT 'confirmed'::text NOT NULL,
    organizer text,
    attendees jsonb,
    recurrence jsonb,
    html_link text,
    etag text,
    remote_updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.commitments (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    memory_id uuid,
    source_vault_file_id uuid,
    text text NOT NULL,
    normalized_text text NOT NULL,
    due_at timestamp with time zone,
    state text DEFAULT 'open'::text NOT NULL,
    surfaced_count integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    done_at timestamp with time zone,
    CONSTRAINT commitments_state_check CHECK ((state = ANY (ARRAY['open'::text, 'done'::text, 'dropped'::text])))
);
CREATE TABLE public.community_blocks (
    blocker_profile_id uuid NOT NULL,
    blocked_profile_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT community_blocks_check CHECK ((blocker_profile_id <> blocked_profile_id))
);
CREATE TABLE public.community_daily_posts (
    day date NOT NULL,
    status text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    post_id uuid,
    theme_id text,
    topic_key text,
    source_kind text,
    source_urls jsonb DEFAULT '[]'::jsonb NOT NULL,
    model text,
    last_error text,
    claimed_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT community_daily_posts_source_kind_check CHECK ((source_kind = ANY (ARRAY['curated'::text, 'trend'::text, 'community'::text]))),
    CONSTRAINT community_daily_posts_status_check CHECK ((status = ANY (ARRAY['claimed'::text, 'published'::text, 'failed'::text, 'skipped'::text])))
);
CREATE TABLE public.community_posts (
    id uuid NOT NULL,
    parent_id uuid,
    author_kind text NOT NULL,
    author_profile_id uuid,
    title text,
    body text NOT NULL,
    status text NOT NULL,
    moderation_category text,
    moderation_attempts integer DEFAULT 0 NOT NULL,
    moderated_at timestamp with time zone,
    hidden_reason text,
    reply_count integer DEFAULT 0 NOT NULL,
    reaction_count integer DEFAULT 0 NOT NULL,
    last_activity_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT community_posts_author_kind_check CHECK ((author_kind = ANY (ARRAY['user'::text, 'ai'::text]))),
    CONSTRAINT community_posts_body_check CHECK ((char_length(body) <= 4000)),
    CONSTRAINT community_posts_check CHECK (((author_kind = 'user'::text) = (author_profile_id IS NOT NULL))),
    CONSTRAINT community_posts_check1 CHECK (((parent_id IS NULL) = (title IS NOT NULL))),
    CONSTRAINT community_posts_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'visible'::text, 'rejected'::text, 'hidden'::text, 'deleted'::text]))),
    CONSTRAINT community_posts_title_check CHECK (((title IS NULL) OR (char_length(title) <= 120)))
);
CREATE TABLE public.community_profiles (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    handle text NOT NULL,
    display_name text NOT NULL,
    terms_version integer NOT NULL,
    terms_accepted_at timestamp with time zone NOT NULL,
    posting_suspended_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.community_reports (
    id uuid NOT NULL,
    post_id uuid NOT NULL,
    reporter_profile_id uuid NOT NULL,
    reason text NOT NULL,
    note text,
    status text DEFAULT 'open'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    resolved_at timestamp with time zone,
    resolved_by_user_id uuid,
    CONSTRAINT community_reports_note_check CHECK (((note IS NULL) OR (char_length(note) <= 500))),
    CONSTRAINT community_reports_reason_check CHECK ((reason = ANY (ARRAY['spam'::text, 'harassment'::text, 'hate'::text, 'sexual'::text, 'violence'::text, 'self_harm'::text, 'misinformation'::text, 'other'::text]))),
    CONSTRAINT community_reports_status_check CHECK ((status = ANY (ARRAY['open'::text, 'actioned'::text, 'dismissed'::text])))
);
CREATE TABLE public.connection_diagnostic_events (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    kind text NOT NULL,
    connection_id text,
    connection_title text,
    severity text NOT NULL,
    message text NOT NULL,
    code text,
    created_at timestamp with time zone
);
CREATE TABLE public.conversation_messages (
    id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    role text NOT NULL,
    content text NOT NULL,
    source_memory_ids uuid[] DEFAULT '{}'::uuid[] NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    parallel_execution_id uuid,
    local_execution_id uuid,
    tool_call_count integer DEFAULT 0 NOT NULL,
    hermes_run_id uuid,
    origin text,
    source_label text
);
CREATE TABLE public.conversations (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    title text NOT NULL,
    space_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    pinned_memory_ids uuid[] DEFAULT '{}'::uuid[] NOT NULL,
    route_provider text,
    route_model text,
    system_key text
);
CREATE TABLE public.cost_ledger (
    tenant_id uuid NOT NULL,
    day date NOT NULL,
    provider text NOT NULL,
    usd_micros bigint DEFAULT 0 NOT NULL,
    calls bigint DEFAULT 0 NOT NULL
);
CREATE TABLE public.device_command_queue (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    kind text NOT NULL,
    domain text,
    payload text NOT NULL,
    created_at timestamp with time zone,
    expires_at timestamp with time zone NOT NULL,
    delivered_at timestamp with time zone,
    result_ok boolean,
    result_error text
);
CREATE TABLE public.device_tokens (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    token text NOT NULL,
    platform text NOT NULL,
    last_seen_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    bundle_id text
);
CREATE TABLE public.email_verification_tokens (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    code_hash text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    used_at timestamp with time zone,
    failed_attempts bigint DEFAULT 0 NOT NULL,
    locked_until timestamp with time zone,
    created_at timestamp with time zone
);
CREATE TABLE public.embedding_reembed_progress (
    target text NOT NULL,
    last_id text,
    rows_done bigint DEFAULT 0 NOT NULL,
    rows_failed bigint DEFAULT 0 NOT NULL,
    completed_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.embedding_usage (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    year_month text NOT NULL,
    tokens_used bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.health_events (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    event_type text NOT NULL,
    value_numeric double precision,
    value_text text,
    unit text,
    recorded_at timestamp with time zone NOT NULL,
    source text,
    metadata jsonb,
    created_at timestamp with time zone
);
CREATE TABLE public.hermes_artifacts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    kind text NOT NULL,
    value text NOT NULL,
    href text NOT NULL,
    label text NOT NULL,
    session_id text NOT NULL,
    session_title text NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    content_hash text NOT NULL,
    collected_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.hermes_gateway_apply_jobs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    state text NOT NULL,
    steps_json text DEFAULT '[]'::text NOT NULL,
    error_message text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.hermes_job_runs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    hermes_job_id text NOT NULL,
    hermes_run_key text NOT NULL,
    status text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    finished_at timestamp with time zone,
    output text,
    error text,
    tokens jsonb,
    vault_file_id uuid,
    skill_run_log_id uuid,
    collected_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.hermes_mirror_state (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    last_sync_at timestamp with time zone,
    last_status text DEFAULT 'never'::text NOT NULL,
    last_error text,
    skills_count bigint DEFAULT 0 NOT NULL,
    jobs_count bigint DEFAULT 0 NOT NULL,
    vault_files_count bigint DEFAULT 0 NOT NULL,
    vault_path text,
    vault_state text DEFAULT 'absent'::text NOT NULL,
    vault_cursor text,
    sessions_cursor text,
    sessions_imported bigint DEFAULT 0 NOT NULL,
    compile_job_id text,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.hermes_mirror_webhooks (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    token text NOT NULL,
    secret_ciphertext bytea NOT NULL,
    secret_nonce bytea NOT NULL,
    rotated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.hermes_mirrored_jobs (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    hermes_job_id text NOT NULL,
    name text,
    schedule text,
    prompt text,
    paused boolean DEFAULT false NOT NULL,
    last_run_at timestamp with time zone,
    next_run_at timestamp with time zone,
    raw jsonb NOT NULL,
    updated_at timestamp with time zone,
    runs_high_water_at timestamp with time zone,
    runs_collected_at timestamp with time zone
);
CREATE TABLE public.hermes_mirrored_skills (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    source text DEFAULT 'custom'::text NOT NULL,
    content_hash text,
    updated_at timestamp with time zone
);
CREATE TABLE public.hermes_profiles (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    hermes_profile_id text NOT NULL,
    status text NOT NULL,
    last_error text,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.hermes_run_events (
    id uuid NOT NULL,
    run_id uuid NOT NULL,
    seq integer NOT NULL,
    event text NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    at timestamp with time zone NOT NULL
);
CREATE TABLE public.hermes_runs (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    hermes_run_id text NOT NULL,
    status text DEFAULT 'queued'::text NOT NULL,
    prompt text NOT NULL,
    session_id text,
    model text,
    conversation_id uuid,
    started_at timestamp with time zone NOT NULL,
    finished_at timestamp with time zone,
    last_event text,
    last_seq integer DEFAULT 0 NOT NULL,
    pending_approval jsonb,
    summary text,
    error text,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.hermes_tenant_containers (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    container_name text NOT NULL,
    port integer NOT NULL,
    api_server_key_ciphertext bytea NOT NULL,
    api_server_key_nonce bytea NOT NULL,
    xai_connected_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_used_at timestamp with time zone,
    nous_connected_at timestamp with time zone
);
CREATE TABLE public.hermes_update_jobs (
    id uuid NOT NULL,
    state text NOT NULL,
    steps_json text NOT NULL,
    from_version text,
    to_version text,
    error_message text,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.idempotency_keys (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    key uuid NOT NULL,
    request_hash text NOT NULL,
    response_status integer NOT NULL,
    response_content_type text,
    response_body bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL
);
CREATE TABLE public.import_items (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    session_id uuid NOT NULL,
    vault_file_id uuid,
    url text,
    title text,
    proposed_space text,
    status text NOT NULL,
    created_at timestamp with time zone
);
CREATE TABLE public.import_sessions (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    source_type text NOT NULL,
    status text NOT NULL,
    total_items bigint DEFAULT 0 NOT NULL,
    staged_items bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.ingestion_batches (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    space_id uuid,
    state text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.ingestion_events (
    id bigint NOT NULL,
    tenant_id uuid NOT NULL,
    batch_id uuid NOT NULL,
    item_id uuid,
    type text NOT NULL,
    state text,
    uploaded_bytes bigint,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE SEQUENCE public.ingestion_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
ALTER SEQUENCE public.ingestion_events_id_seq OWNED BY public.ingestion_events.id;
CREATE TABLE public.ingestion_items (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    batch_id uuid NOT NULL,
    kind text NOT NULL,
    state text NOT NULL,
    file_name text,
    content_type text,
    size_bytes bigint,
    uploaded_bytes bigint DEFAULT 0 NOT NULL,
    expected_sha256 text,
    url text,
    vault_file_id uuid,
    memory_id uuid,
    summary text,
    error_message text,
    credibility jsonb,
    attempts integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    next_attempt_at timestamp with time zone,
    lease_expires_at timestamp with time zone,
    source_token_hash text,
    source_token_expires_at timestamp with time zone,
    content_sha256 text,
    pipeline_version text DEFAULT 'multimodal-v2'::text NOT NULL,
    reused_from_item_id uuid,
    graph_ready_at timestamp with time zone,
    terminal_notified_at timestamp with time zone,
    CONSTRAINT ingestion_items_kind_check CHECK ((kind = ANY (ARRAY['file'::text, 'url'::text])))
);
CREATE TABLE public.insights (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    section text NOT NULL,
    headline text NOT NULL,
    summary text NOT NULL,
    source_memory_ids uuid[] DEFAULT '{}'::uuid[] NOT NULL,
    period_start timestamp with time zone,
    period_end timestamp with time zone,
    dismissed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    knowledge_edge_id uuid
);
CREATE TABLE public.kanban_boards (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    title text NOT NULL,
    version bigint DEFAULT 0 NOT NULL,
    archived_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    created_by_user_id uuid,
    updated_by_user_id uuid
);
CREATE TABLE public.kanban_cards (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    board_id uuid NOT NULL,
    column_id uuid NOT NULL,
    title text NOT NULL,
    body text,
    rank text NOT NULL,
    priority text,
    due_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    extra jsonb,
    created_by_user_id uuid,
    updated_by_user_id uuid
);
CREATE TABLE public.kanban_columns (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    board_id uuid NOT NULL,
    title text NOT NULL,
    rank text NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.kb_compile_reject_list (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    content_hash text NOT NULL,
    vault_file_id uuid,
    rejected_at timestamp with time zone NOT NULL
);
CREATE TABLE public.knowledge_edges (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    from_node_id uuid NOT NULL,
    to_node_id uuid NOT NULL,
    predicate text NOT NULL,
    state text NOT NULL,
    confidence double precision NOT NULL,
    rationale text,
    counter_evidence text,
    evidence_fingerprint text NOT NULL,
    review_note text,
    reviewed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT knowledge_edges_check CHECK ((from_node_id <> to_node_id)),
    CONSTRAINT knowledge_edges_confidence_check CHECK (((confidence >= (0)::double precision) AND (confidence <= (1)::double precision))),
    CONSTRAINT knowledge_edges_predicate_check CHECK ((predicate = ANY (ARRAY['mentions'::text, 'about'::text, 'supports'::text, 'contradicts'::text, 'causes'::text, 'precedes'::text, 'related_to'::text, 'derived_from'::text]))),
    CONSTRAINT knowledge_edges_state_check CHECK ((state = ANY (ARRAY['asserted'::text, 'suggested'::text, 'confirmed'::text, 'dismissed'::text, 'stale'::text])))
);
CREATE TABLE public.knowledge_evidence (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    node_id uuid,
    edge_id uuid,
    memory_id uuid NOT NULL,
    source_vault_file_id uuid,
    quote text NOT NULL,
    start_offset integer,
    end_offset integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT knowledge_evidence_check CHECK (((node_id IS NOT NULL) <> (edge_id IS NOT NULL))),
    CONSTRAINT knowledge_evidence_check1 CHECK (((end_offset IS NULL) OR (end_offset >= start_offset))),
    CONSTRAINT knowledge_evidence_start_offset_check CHECK (((start_offset IS NULL) OR (start_offset >= 0)))
);
CREATE TABLE public.knowledge_extraction_jobs (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    memory_id uuid NOT NULL,
    content_fingerprint text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    last_error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT knowledge_extraction_jobs_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'processing'::text, 'retry'::text, 'completed'::text])))
);
CREATE TABLE public.knowledge_nodes (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    kind text NOT NULL,
    canonical_key text NOT NULL,
    label text NOT NULL,
    summary text,
    occurred_at timestamp with time zone,
    confidence double precision NOT NULL,
    extractor_provider text,
    extractor_model text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT knowledge_nodes_confidence_check CHECK (((confidence >= (0)::double precision) AND (confidence <= (1)::double precision))),
    CONSTRAINT knowledge_nodes_kind_check CHECK ((kind = ANY (ARRAY['claim'::text, 'entity'::text, 'event'::text])))
);
CREATE TABLE public.learned_digest_log (
    tenant_id uuid NOT NULL,
    week_start date NOT NULL,
    sent_at timestamp with time zone DEFAULT now() NOT NULL,
    outcome text DEFAULT 'sent'::text NOT NULL
);
CREATE TABLE public.marketplace_executions (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    install_id uuid NOT NULL,
    version_id uuid NOT NULL,
    tool_name text NOT NULL,
    status text NOT NULL,
    error_code text,
    duration_ms bigint,
    created_at timestamp with time zone
);
CREATE TABLE public.marketplace_listings (
    id uuid NOT NULL,
    publisher_id uuid NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    summary text NOT NULL,
    description text NOT NULL,
    category text NOT NULL,
    icon_url text,
    screenshots text[] NOT NULL,
    status text NOT NULL,
    featured boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.marketplace_publishers (
    id uuid NOT NULL,
    owner_user_id uuid NOT NULL,
    handle text NOT NULL,
    display_name text NOT NULL,
    bio text,
    website_url text,
    status text NOT NULL,
    verified boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.marketplace_ratings (
    id uuid NOT NULL,
    listing_id uuid NOT NULL,
    user_id uuid NOT NULL,
    rating bigint NOT NULL,
    body text,
    moderation_status text NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.marketplace_submissions (
    id uuid NOT NULL,
    version_id uuid NOT NULL,
    publisher_user_id uuid NOT NULL,
    status text NOT NULL,
    validation_errors text[] NOT NULL,
    review_note text,
    reviewed_by_user_id uuid,
    submitted_at timestamp with time zone,
    reviewed_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.marketplace_versions (
    id uuid NOT NULL,
    listing_id uuid NOT NULL,
    version text NOT NULL,
    status text NOT NULL,
    runtime_kind text NOT NULL,
    permissions text[] NOT NULL,
    network_hosts text[] NOT NULL,
    config_fields jsonb NOT NULL,
    changelog text,
    artifact_key text,
    artifact_sha256 text,
    artifact_signature text,
    manifest_json bytea,
    published_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.meeting_briefs (
    tenant_id uuid NOT NULL,
    event_id uuid NOT NULL,
    claimed_at timestamp with time zone DEFAULT now() NOT NULL,
    sent_at timestamp with time zone,
    outcome text DEFAULT 'claimed'::text NOT NULL,
    memories jsonb DEFAULT '[]'::jsonb NOT NULL
);
CREATE TABLE public.memories (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    content text NOT NULL,
    created_at timestamp with time zone,
    embedding public.vector(1536),
    tags text[],
    score double precision DEFAULT 0 NOT NULL,
    access_count bigint DEFAULT 0 NOT NULL,
    query_hit_count bigint DEFAULT 0 NOT NULL,
    last_accessed_at timestamp with time zone,
    source_vault_file_id uuid,
    lat double precision,
    lng double precision,
    accuracy_m double precision,
    place_name text,
    content_tsv tsvector GENERATED ALWAYS AS (to_tsvector('english'::regconfig, content)) STORED,
    review_state text DEFAULT 'auto'::text NOT NULL,
    space_id uuid,
    graph_x double precision,
    graph_y double precision,
    graph_z double precision,
    graph_layout_at timestamp with time zone,
    created_by_user_id uuid,
    updated_by_user_id uuid,
    origin_kind text DEFAULT 'legacy'::text NOT NULL,
    origin_source_id text,
    origin_provider text,
    origin_model text,
    origin_conversation_message_id uuid,
    last_reviewed_at timestamp with time zone,
    review_count bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone,
    kind text,
    next_review_at timestamp with time zone,
    resurface_muted boolean DEFAULT false NOT NULL,
    CONSTRAINT memories_kind_check CHECK ((kind = ANY (ARRAY['preference'::text, 'decision'::text, 'fact'::text, 'note'::text])))
);
CREATE TABLE public.memories_archive (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    content text NOT NULL,
    tags text[],
    embedding public.vector(1536),
    score double precision DEFAULT 0 NOT NULL,
    access_count bigint DEFAULT 0 NOT NULL,
    query_hit_count bigint DEFAULT 0 NOT NULL,
    last_accessed_at timestamp with time zone,
    created_at timestamp with time zone,
    archived_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.memory_chunks (
    chunk_id text NOT NULL,
    document_id text NOT NULL,
    tenant_id uuid NOT NULL,
    memory_id uuid NOT NULL,
    vault_file_id uuid,
    space_id uuid,
    source_path text,
    ordinal integer NOT NULL,
    heading_path jsonb DEFAULT '[]'::jsonb NOT NULL,
    start_line integer NOT NULL,
    end_line integer NOT NULL,
    text text NOT NULL,
    content_sha256 text NOT NULL,
    embedding public.vector(1536),
    content_tsv tsvector GENERATED ALWAYS AS (to_tsvector('english'::regconfig, text)) STORED,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.memory_contributions (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    memory_id uuid NOT NULL,
    operation text NOT NULL,
    actor_kind text NOT NULL,
    source_kind text NOT NULL,
    provider text,
    model text,
    source_reference text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.memory_index_jobs (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    source_kind text NOT NULL,
    source_id text NOT NULL,
    conversation_message_id uuid,
    content text NOT NULL,
    provider text,
    model text,
    status text DEFAULT 'pending'::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    last_error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.memory_sync_tombstones (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    memory_id uuid NOT NULL,
    deleted_at timestamp with time zone
);
CREATE TABLE public.mfa_challenges (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    purpose text NOT NULL,
    channel text NOT NULL,
    destination text NOT NULL,
    code_hash text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    failed_attempts bigint DEFAULT 0 NOT NULL,
    resend_count bigint DEFAULT 0 NOT NULL,
    last_sent_at timestamp with time zone,
    created_at timestamp with time zone
);
CREATE TABLE public.nvidia_batch_jobs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    skill_ref text NOT NULL,
    state text NOT NULL,
    steps_json text DEFAULT '[]'::text NOT NULL,
    error_message text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.oauth_identities (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    provider text NOT NULL,
    provider_user_id text NOT NULL,
    email text NOT NULL,
    email_verified boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone
);
CREATE TABLE public.onboarding_state (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    signup_completed boolean DEFAULT false NOT NULL,
    signup_completed_at timestamp with time zone,
    email_verified_completed boolean DEFAULT false NOT NULL,
    email_verified_completed_at timestamp with time zone,
    soul_configured_completed boolean DEFAULT false NOT NULL,
    soul_configured_completed_at timestamp with time zone,
    first_capture_completed boolean DEFAULT false NOT NULL,
    first_capture_completed_at timestamp with time zone,
    first_kb_compile_completed boolean DEFAULT false NOT NULL,
    first_kb_compile_completed_at timestamp with time zone,
    first_query_completed boolean DEFAULT false NOT NULL,
    first_query_completed_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    first_memory_compile_completed boolean DEFAULT false NOT NULL,
    first_memory_compile_completed_at timestamp with time zone,
    brain_configured_completed boolean DEFAULT false NOT NULL,
    brain_configured_completed_at timestamp with time zone,
    guided_start_dismissed_at timestamp with time zone
);
CREATE TABLE public.password_reset_tokens (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    code_hash text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    used_at timestamp with time zone,
    failed_attempts bigint DEFAULT 0 NOT NULL,
    locked_until timestamp with time zone,
    created_at timestamp with time zone
);
CREATE TABLE public.photo_index (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    asset_local_id text NOT NULL,
    taken_at timestamp with time zone,
    is_screenshot boolean DEFAULT false NOT NULL,
    ocr_text text,
    scene_tags text[],
    embedding public.vector(1536),
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.plugin_installs (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    plugin_slug text NOT NULL,
    status text NOT NULL,
    config_ciphertext bytea NOT NULL,
    config_nonce bytea NOT NULL,
    last_sync_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    marketplace_version_id uuid,
    granted_permissions text[] DEFAULT '{}'::text[] NOT NULL
);
CREATE TABLE public.plugins (
    id uuid NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    summary text NOT NULL,
    category text NOT NULL,
    capability_kind text NOT NULL,
    binding text NOT NULL,
    icon_slug text NOT NULL,
    version text NOT NULL,
    publisher text NOT NULL,
    verified boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone
);
CREATE TABLE public.prepared_local_executions (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    messages jsonb NOT NULL,
    source_ids uuid[] NOT NULL,
    committed_message_id uuid,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone,
    user_message_id uuid
);
CREATE TABLE public.proactive_push_log (
    tenant_id uuid NOT NULL,
    day date NOT NULL,
    kind text NOT NULL,
    sent_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.projects (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    description text,
    archived boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.provider_failover_events (
    id uuid NOT NULL,
    tenant_id uuid,
    provider text NOT NULL,
    model text,
    status_code bigint,
    error_code text,
    fallback_provider text,
    fallback_model text,
    source text NOT NULL,
    happened_at timestamp with time zone NOT NULL
);
CREATE TABLE public.recap_themes (
    user_id uuid NOT NULL,
    period_key text NOT NULL,
    status text NOT NULL,
    headline text,
    sentence text,
    themes text[] DEFAULT '{}'::text[] NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    requested_at timestamp with time zone,
    generated_at timestamp with time zone,
    CONSTRAINT recap_themes_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'ready'::text, 'fallback'::text])))
);
CREATE TABLE public.refresh_tokens (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    token_hash text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone
);
CREATE TABLE public.reminders (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    title text NOT NULL,
    body text NOT NULL,
    fire_at timestamp with time zone NOT NULL,
    recurrence_cron text,
    fired_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.retrieval_leak_reports (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    period_start timestamp with time zone NOT NULL,
    period_end timestamp with time zone NOT NULL,
    total_retrievals integer NOT NULL,
    zero_hit_count integer NOT NULL,
    zero_hit_rate double precision NOT NULL,
    mean_top_distance double precision,
    worst_source text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.retrieval_telemetry_events (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    source_path text NOT NULL,
    space_id uuid,
    hit_count integer NOT NULL,
    zero_hit boolean NOT NULL,
    top_distance double precision,
    mean_distance double precision,
    limit_requested integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.router_attempts (
    id uuid NOT NULL,
    execution_id uuid NOT NULL,
    ordinal integer NOT NULL,
    role text NOT NULL,
    provider text NOT NULL,
    model text NOT NULL,
    outcome text NOT NULL,
    error_code text,
    tokens_in bigint DEFAULT 0 NOT NULL,
    tokens_out bigint DEFAULT 0 NOT NULL,
    estimated_cost_usd_micros bigint DEFAULT 0 NOT NULL,
    latency_ms bigint DEFAULT 0 NOT NULL,
    usage_estimated boolean DEFAULT true NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.router_bindings (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    scope text NOT NULL,
    scope_id text NOT NULL,
    profile_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.router_executions (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    profile_id uuid,
    rule_id uuid,
    surface text NOT NULL,
    task_type text NOT NULL,
    strategy text NOT NULL,
    status text NOT NULL,
    selected_provider text,
    selected_model text,
    tokens_in bigint DEFAULT 0 NOT NULL,
    tokens_out bigint DEFAULT 0 NOT NULL,
    estimated_cost_usd_micros bigint DEFAULT 0 NOT NULL,
    latency_ms bigint DEFAULT 0 NOT NULL,
    usage_estimated boolean DEFAULT true NOT NULL,
    fallback_count integer DEFAULT 0 NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    conversation_id uuid,
    space_id uuid,
    prompt text,
    parallel_strategy text,
    participant_count integer DEFAULT 0 NOT NULL,
    synthesized_answer text,
    degraded boolean DEFAULT false NOT NULL,
    vault_id uuid,
    actor_user_id uuid
);
CREATE TABLE public.router_monthly_usage (
    tenant_id uuid NOT NULL,
    month date NOT NULL,
    managed_usd_micros bigint DEFAULT 0 NOT NULL,
    byok_usd_micros bigint DEFAULT 0 NOT NULL,
    reserved_usd_micros bigint DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    successful_requests bigint DEFAULT 0 NOT NULL,
    fallback_count bigint DEFAULT 0 NOT NULL,
    tokens_in bigint DEFAULT 0 NOT NULL,
    tokens_out bigint DEFAULT 0 NOT NULL,
    latency_ms_total bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.router_outputs (
    id uuid NOT NULL,
    execution_id uuid NOT NULL,
    participant_id uuid,
    role text NOT NULL,
    provider text NOT NULL,
    model text NOT NULL,
    stage text NOT NULL,
    round integer NOT NULL,
    content text NOT NULL,
    status text NOT NULL,
    tokens_in bigint DEFAULT 0 NOT NULL,
    tokens_out bigint DEFAULT 0 NOT NULL,
    estimated_cost_usd_micros bigint DEFAULT 0 NOT NULL,
    latency_ms bigint DEFAULT 0 NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.router_profiles (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    mode text NOT NULL,
    is_preset boolean DEFAULT false NOT NULL,
    document jsonb NOT NULL,
    revision integer DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.router_synthesis_presets (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    prompt text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.self_improvement_changes (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    run_id uuid,
    kind text NOT NULL,
    state text NOT NULL,
    trigger text NOT NULL,
    title text NOT NULL,
    summary text NOT NULL,
    patch text,
    proposed_markdown text,
    base_sha256 text,
    report_markdown text,
    failure_reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    decided_at timestamp with time zone,
    applied_at timestamp with time zone
);
CREATE TABLE public.self_improvement_runs (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    kind text NOT NULL,
    status text NOT NULL,
    trigger text NOT NULL,
    dry_run boolean DEFAULT true NOT NULL,
    model_used text,
    report_markdown text,
    snapshot_json text,
    actions_applied integer DEFAULT 0 NOT NULL,
    actions_skipped integer DEFAULT 0 NOT NULL,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    failure_reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.self_improvement_settings (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    curator_enabled boolean DEFAULT true NOT NULL,
    interval_hours integer DEFAULT 168 NOT NULL,
    minimum_idle_hours integer DEFAULT 2 NOT NULL,
    consolidate boolean DEFAULT true NOT NULL,
    prune_builtins boolean DEFAULT false NOT NULL,
    backup_keep integer DEFAULT 5 NOT NULL,
    soul_review_enabled boolean DEFAULT true NOT NULL,
    review_complex_sessions boolean DEFAULT true NOT NULL,
    soul_review_window_days integer DEFAULT 14 NOT NULL,
    soul_review_cooldown_hours integer DEFAULT 24 NOT NULL,
    model_mode text DEFAULT 'economy'::text NOT NULL,
    last_activity_at timestamp with time zone,
    last_curator_review_at timestamp with time zone,
    last_soul_review_at timestamp with time zone,
    next_review_at timestamp with time zone,
    lease_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.skill_run_log (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    source text NOT NULL,
    name text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    ended_at timestamp with time zone,
    status text NOT NULL,
    error text,
    model_used text,
    mtok_in integer DEFAULT 0 NOT NULL,
    mtok_out integer DEFAULT 0 NOT NULL,
    markdown text,
    blocks jsonb,
    space_id uuid
);
CREATE TABLE public.skills_state (
    tenant_id uuid NOT NULL,
    source text NOT NULL,
    name text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    schedule_override text,
    last_run_at timestamp with time zone,
    last_status text,
    last_error text,
    daily_run_count integer DEFAULT 0 NOT NULL,
    daily_run_reset_at timestamp with time zone,
    apns_category text,
    domain text,
    space_id uuid,
    run_at timestamp with time zone,
    workflow_id uuid,
    curator_pinned boolean DEFAULT false NOT NULL,
    curator_state text DEFAULT 'active'::text NOT NULL,
    curator_last_activity_at timestamp with time zone,
    curator_archived_at timestamp with time zone,
    origin_kind text,
    origin_id uuid,
    CONSTRAINT skills_state_origin_kind_check CHECK ((origin_kind = ANY (ARRAY['conversation'::text, 'card'::text]))),
    CONSTRAINT skills_state_source_check CHECK ((source = ANY (ARRAY['builtin'::text, 'vault'::text])))
);
CREATE TABLE public.spaces (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    description text,
    color text,
    icon text,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    category text,
    note_count integer DEFAULT 0 NOT NULL,
    last_compiled_at timestamp with time zone,
    created_by_user_id uuid,
    updated_by_user_id uuid
);
CREATE TABLE public.team_invitations (
    id uuid NOT NULL,
    team_id uuid NOT NULL,
    email text NOT NULL,
    token_hash text NOT NULL,
    vault_grants text NOT NULL,
    invited_by_user_id uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    accepted_at timestamp with time zone,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone
);
CREATE TABLE public.team_memberships (
    id uuid NOT NULL,
    team_id uuid NOT NULL,
    user_id uuid NOT NULL,
    role text NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    CONSTRAINT team_memberships_role_check CHECK ((role = ANY (ARRAY['owner'::text, 'admin'::text, 'member'::text])))
);
CREATE TABLE public.teams (
    id uuid NOT NULL,
    name text NOT NULL,
    owner_user_id uuid NOT NULL,
    billing_sponsor_user_id uuid NOT NULL,
    archived_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.usage_events (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    metric text NOT NULL,
    amount bigint NOT NULL,
    source text NOT NULL,
    idempotency_key text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT usage_events_amount_check CHECK ((amount >= 0)),
    CONSTRAINT usage_events_metric_check CHECK ((metric = ANY (ARRAY['memory_compile_run'::text, 'memory_compile_file'::text, 'voice_transcribe'::text, 'voice_speech'::text])))
);
CREATE TABLE public.usage_meter (
    tenant_id uuid NOT NULL,
    day date NOT NULL,
    model text NOT NULL,
    mtok_in bigint DEFAULT 0 NOT NULL,
    mtok_out bigint DEFAULT 0 NOT NULL,
    chars_out bigint DEFAULT 0 NOT NULL
);
CREATE TABLE public.user_chat_preferences (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    auto_expand_thinking boolean DEFAULT true NOT NULL,
    send_on_return boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    hybrid_profile text DEFAULT 'balanced'::text NOT NULL,
    local_fallback_enabled boolean DEFAULT true NOT NULL,
    cloud_fallback_enabled boolean DEFAULT true NOT NULL,
    sync_local_conversations boolean DEFAULT true NOT NULL
);
CREATE TABLE public.user_hermes_config (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    base_url text NOT NULL,
    auth_header_ciphertext bytea,
    auth_header_nonce bytea,
    verified_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    name text,
    cron_dashboard_url text,
    cron_dashboard_token_ciphertext bytea,
    cron_dashboard_token_nonce bytea,
    capabilities text,
    capabilities_checked_at timestamp with time zone
);
CREATE TABLE public.user_hermes_gateways (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    gateway_id text NOT NULL,
    config_ciphertext bytea NOT NULL,
    config_nonce bytea NOT NULL,
    status text NOT NULL,
    verified_at timestamp with time zone,
    last_failure_at timestamp with time zone,
    last_failure_code text,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.user_hermes_profiles (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    slug text NOT NULL,
    label text NOT NULL,
    system_prompt text NOT NULL,
    is_default boolean DEFAULT false NOT NULL,
    skills_enabled text[] DEFAULT '{}'::text[] NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.user_llm_preferences (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    primary_provider text NOT NULL,
    primary_model text NOT NULL,
    fallback_chain jsonb NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    mode text DEFAULT 'managed'::text NOT NULL,
    allowed_providers jsonb,
    blocked_providers jsonb,
    CONSTRAINT user_llm_preferences_mode_check CHECK ((mode = ANY (ARRAY['managed'::text, 'byok'::text])))
);
CREATE TABLE public.user_provider_credential_pool_keys (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    provider text NOT NULL,
    ciphertext bytea,
    nonce bytea,
    label text,
    created_at timestamp with time zone
);
CREATE TABLE public.user_provider_credentials (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    provider text NOT NULL,
    credential_kind text NOT NULL,
    ciphertext bytea,
    nonce bytea,
    base_url text,
    label text,
    verified_at timestamp with time zone,
    last_failure_at timestamp with time zone,
    last_failure_code text,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    supports_tools boolean,
    tools_probed_at timestamp with time zone
);
CREATE TABLE public.users (
    id uuid NOT NULL,
    email text NOT NULL,
    password_hash text NOT NULL,
    is_verified boolean DEFAULT false NOT NULL,
    failed_login_attempts bigint DEFAULT 0 NOT NULL,
    lockout_until timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    username text NOT NULL,
    tier text DEFAULT 'trial'::text NOT NULL,
    tier_expires_at timestamp with time zone,
    tier_override text DEFAULT 'none'::text NOT NULL,
    revenuecat_user_id text,
    context_routing boolean DEFAULT false NOT NULL,
    privacy_no_cn_origin boolean DEFAULT false NOT NULL,
    timezone text DEFAULT 'UTC'::text NOT NULL,
    is_admin boolean DEFAULT false NOT NULL,
    vault_initialized boolean DEFAULT false NOT NULL,
    auto_save_links boolean DEFAULT true NOT NULL,
    mnemosyne_enabled boolean DEFAULT true NOT NULL,
    hermes_audio_token_epoch timestamp with time zone,
    last_location_lat double precision,
    last_location_lng double precision,
    last_location_place text,
    last_location_at timestamp with time zone,
    CONSTRAINT users_tier_check CHECK ((tier = ANY (ARRAY['free'::text, 'trial'::text, 'pro'::text, 'ultimate'::text, 'lapsed'::text, 'archived'::text]))),
    CONSTRAINT users_tier_override_check CHECK ((tier_override = ANY (ARRAY['none'::text, 'pro'::text, 'ultimate'::text])))
);
CREATE TABLE public.vault_activity_events (
    id uuid NOT NULL,
    vault_id uuid NOT NULL,
    actor_user_id uuid,
    actor_name text NOT NULL,
    action text NOT NULL,
    target_type text NOT NULL,
    target_id uuid,
    target_title text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.vault_files (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    space_id uuid,
    path text NOT NULL,
    content_type text NOT NULL,
    size_bytes bigint NOT NULL,
    sha256 text NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    processed_at timestamp with time zone,
    metadata jsonb,
    created_by_user_id uuid,
    updated_by_user_id uuid
);
CREATE TABLE public.vault_links (
    link_id bigint NOT NULL,
    tenant_id uuid NOT NULL,
    source_vault_file_id uuid NOT NULL,
    source_path text,
    source_line integer NOT NULL,
    raw_target text NOT NULL,
    target_slug text NOT NULL,
    target_heading text,
    label text,
    target_vault_file_id uuid,
    resolved boolean DEFAULT false NOT NULL,
    resolution_state text DEFAULT 'unresolved'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE SEQUENCE public.vault_links_link_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
ALTER SEQUENCE public.vault_links_link_id_seq OWNED BY public.vault_links.link_id;
CREATE TABLE public.vault_memberships (
    id uuid NOT NULL,
    vault_id uuid NOT NULL,
    user_id uuid NOT NULL,
    role text NOT NULL,
    can_use_ai boolean DEFAULT false NOT NULL,
    created_by_user_id uuid NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    CONSTRAINT vault_memberships_role_check CHECK ((role = ANY (ARRAY['viewer'::text, 'editor'::text, 'admin'::text])))
);
CREATE TABLE public.vaults (
    id uuid NOT NULL,
    team_id uuid,
    personal_owner_user_id uuid,
    name text NOT NULL,
    archived_at timestamp with time zone,
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    CONSTRAINT vault_owner_kind CHECK ((((team_id IS NOT NULL) AND (personal_owner_user_id IS NULL)) OR ((team_id IS NULL) AND (personal_owner_user_id IS NOT NULL))))
);
CREATE TABLE public.webauthn_credentials (
    id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    credential_id text NOT NULL,
    public_key bytea NOT NULL,
    sign_count bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone,
    updated_at timestamp with time zone
);
CREATE TABLE public.workflow_approvals (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    run_id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    node_id uuid NOT NULL,
    title text NOT NULL,
    message text,
    status text DEFAULT 'pending'::text NOT NULL,
    decision_note text,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    memory_ids uuid[] DEFAULT '{}'::uuid[] NOT NULL
);
CREATE TABLE public.workflow_node_runs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    run_id uuid NOT NULL,
    node_id uuid NOT NULL,
    node_name text NOT NULL,
    status text NOT NULL,
    attempt integer DEFAULT 1 NOT NULL,
    input_snapshot jsonb,
    output_snapshot jsonb,
    error_message text,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    selected_provider text,
    selected_model text,
    tokens_in bigint,
    tokens_out bigint,
    managed_cost_usd_micros bigint
);
CREATE TABLE public.workflow_run_events (
    sequence bigint NOT NULL,
    tenant_id uuid NOT NULL,
    run_id uuid NOT NULL,
    kind text NOT NULL,
    node_id uuid,
    message text,
    data jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE SEQUENCE public.workflow_run_events_sequence_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;
ALTER SEQUENCE public.workflow_run_events_sequence_seq OWNED BY public.workflow_run_events.sequence;
CREATE TABLE public.workflow_runs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    version_id uuid NOT NULL,
    status text NOT NULL,
    trigger_kind text NOT NULL,
    input jsonb DEFAULT '{}'::jsonb NOT NULL,
    conversation_id uuid,
    lease_owner text,
    lease_expires_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    error_message text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    dedupe_key text,
    pause_reason text,
    managed_spend_usd_micros bigint DEFAULT 0 NOT NULL,
    managed_spend_limit_usd_micros bigint DEFAULT 0 NOT NULL,
    lease_heartbeat_at timestamp with time zone
);
CREATE TABLE public.workflow_spend_buckets (
    scope_key text NOT NULL,
    period_kind text NOT NULL,
    period_start date NOT NULL,
    spent_usd_micros bigint DEFAULT 0 NOT NULL,
    reserved_usd_micros bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT workflow_spend_buckets_reserved_usd_micros_check CHECK ((reserved_usd_micros >= 0)),
    CONSTRAINT workflow_spend_buckets_spent_usd_micros_check CHECK ((spent_usd_micros >= 0))
);
CREATE TABLE public.workflow_versions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    version integer NOT NULL,
    definition jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.workflow_webhooks (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    secret_ciphertext bytea NOT NULL,
    secret_nonce bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
CREATE TABLE public.workflows (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    description_text text,
    enabled boolean DEFAULT true NOT NULL,
    draft_definition jsonb NOT NULL,
    draft_revision integer DEFAULT 1 NOT NULL,
    published_version_id uuid,
    is_legacy_job boolean DEFAULT false NOT NULL,
    legacy_skill_name text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);
ALTER TABLE ONLY public.ingestion_events ALTER COLUMN id SET DEFAULT nextval('public.ingestion_events_id_seq'::regclass);
ALTER TABLE ONLY public.vault_links ALTER COLUMN link_id SET DEFAULT nextval('public.vault_links_link_id_seq'::regclass);
ALTER TABLE ONLY public.workflow_run_events ALTER COLUMN sequence SET DEFAULT nextval('public.workflow_run_events_sequence_seq'::regclass);
ALTER TABLE ONLY public.achievement_progress
    ADD CONSTRAINT achievement_progress_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.achievement_progress
    ADD CONSTRAINT achievement_progress_tenant_id_achievement_key_key UNIQUE (tenant_id, achievement_key);
ALTER TABLE ONLY public.agent_connections
    ADD CONSTRAINT agent_connections_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.agent_room_members
    ADD CONSTRAINT agent_room_members_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.agent_room_messages
    ADD CONSTRAINT agent_room_messages_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.agent_rooms
    ADD CONSTRAINT agent_rooms_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.agent_turn_traces
    ADD CONSTRAINT agent_turn_traces_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.analytics_daily_rollups
    ADD CONSTRAINT analytics_daily_rollups_pkey PRIMARY KEY (day, vault_id, actor_user_id, metric, dimension_key);
ALTER TABLE ONLY public.analytics_events
    ADD CONSTRAINT analytics_events_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.analytics_recommendation_states
    ADD CONSTRAINT analytics_recommendation_states_pkey PRIMARY KEY (vault_id, user_id, recommendation_id);
ALTER TABLE ONLY public.apns_category_prefs
    ADD CONSTRAINT apns_category_prefs_pkey PRIMARY KEY (tenant_id);
ALTER TABLE ONLY public.apple_consent
    ADD CONSTRAINT apple_consent_pkey PRIMARY KEY (tenant_id, domain);
ALTER TABLE ONLY public.apple_reminders
    ADD CONSTRAINT apple_reminders_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.billing_event_logs
    ADD CONSTRAINT billing_event_logs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.brief_feedback
    ADD CONSTRAINT brief_feedback_brief_id_card_id_key UNIQUE (brief_id, card_id);
ALTER TABLE ONLY public.brief_feedback
    ADD CONSTRAINT brief_feedback_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.briefs
    ADD CONSTRAINT briefs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.briefs
    ADD CONSTRAINT briefs_tenant_id_date_key UNIQUE (tenant_id, date);
ALTER TABLE ONLY public.calendar_accounts
    ADD CONSTRAINT calendar_accounts_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.calendar_accounts
    ADD CONSTRAINT calendar_accounts_tenant_id_provider_key UNIQUE (tenant_id, provider);
ALTER TABLE ONLY public.calendar_events
    ADD CONSTRAINT calendar_events_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.calendar_events
    ADD CONSTRAINT calendar_events_tenant_id_source_external_id_key UNIQUE (tenant_id, source, external_id);
ALTER TABLE ONLY public.commitments
    ADD CONSTRAINT commitments_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.community_blocks
    ADD CONSTRAINT community_blocks_pkey PRIMARY KEY (blocker_profile_id, blocked_profile_id);
ALTER TABLE ONLY public.community_daily_posts
    ADD CONSTRAINT community_daily_posts_pkey PRIMARY KEY (day);
ALTER TABLE ONLY public.community_posts
    ADD CONSTRAINT community_posts_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.community_profiles
    ADD CONSTRAINT community_profiles_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.community_profiles
    ADD CONSTRAINT community_profiles_user_id_key UNIQUE (user_id);
ALTER TABLE ONLY public.community_reports
    ADD CONSTRAINT community_reports_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.community_reports
    ADD CONSTRAINT community_reports_post_id_reporter_profile_id_key UNIQUE (post_id, reporter_profile_id);
ALTER TABLE ONLY public.connection_diagnostic_events
    ADD CONSTRAINT connection_diagnostic_events_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.conversation_messages
    ADD CONSTRAINT conversation_messages_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.cost_ledger
    ADD CONSTRAINT cost_ledger_pkey PRIMARY KEY (tenant_id, day, provider);
ALTER TABLE ONLY public.device_command_queue
    ADD CONSTRAINT device_command_queue_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.device_tokens
    ADD CONSTRAINT device_tokens_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.email_verification_tokens
    ADD CONSTRAINT email_verification_tokens_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.embedding_reembed_progress
    ADD CONSTRAINT embedding_reembed_progress_pkey PRIMARY KEY (target);
ALTER TABLE ONLY public.embedding_usage
    ADD CONSTRAINT embedding_usage_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.health_events
    ADD CONSTRAINT health_events_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.hermes_artifacts
    ADD CONSTRAINT hermes_artifacts_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.hermes_artifacts
    ADD CONSTRAINT hermes_artifacts_tenant_id_content_hash_key UNIQUE (tenant_id, content_hash);
ALTER TABLE ONLY public.hermes_gateway_apply_jobs
    ADD CONSTRAINT hermes_gateway_apply_jobs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.hermes_job_runs
    ADD CONSTRAINT hermes_job_runs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.hermes_job_runs
    ADD CONSTRAINT hermes_job_runs_tenant_id_hermes_run_key_key UNIQUE (tenant_id, hermes_run_key);
ALTER TABLE ONLY public.hermes_mirror_state
    ADD CONSTRAINT hermes_mirror_state_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.hermes_mirror_webhooks
    ADD CONSTRAINT hermes_mirror_webhooks_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.hermes_mirror_webhooks
    ADD CONSTRAINT hermes_mirror_webhooks_tenant_id_key UNIQUE (tenant_id);
ALTER TABLE ONLY public.hermes_mirror_webhooks
    ADD CONSTRAINT hermes_mirror_webhooks_token_key UNIQUE (token);
ALTER TABLE ONLY public.hermes_mirrored_jobs
    ADD CONSTRAINT hermes_mirrored_jobs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.hermes_mirrored_skills
    ADD CONSTRAINT hermes_mirrored_skills_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.hermes_profiles
    ADD CONSTRAINT hermes_profiles_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.hermes_run_events
    ADD CONSTRAINT hermes_run_events_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.hermes_runs
    ADD CONSTRAINT hermes_runs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.hermes_tenant_containers
    ADD CONSTRAINT hermes_tenant_containers_container_name_key UNIQUE (container_name);
ALTER TABLE ONLY public.hermes_tenant_containers
    ADD CONSTRAINT hermes_tenant_containers_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.hermes_tenant_containers
    ADD CONSTRAINT hermes_tenant_containers_tenant_id_key UNIQUE (tenant_id);
ALTER TABLE ONLY public.hermes_update_jobs
    ADD CONSTRAINT hermes_update_jobs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.idempotency_keys
    ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.import_items
    ADD CONSTRAINT import_items_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.import_sessions
    ADD CONSTRAINT import_sessions_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.ingestion_batches
    ADD CONSTRAINT ingestion_batches_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.ingestion_events
    ADD CONSTRAINT ingestion_events_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.ingestion_items
    ADD CONSTRAINT ingestion_items_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.insights
    ADD CONSTRAINT insights_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.kanban_boards
    ADD CONSTRAINT kanban_boards_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.kanban_cards
    ADD CONSTRAINT kanban_cards_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.kanban_columns
    ADD CONSTRAINT kanban_columns_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.kb_compile_reject_list
    ADD CONSTRAINT kb_compile_reject_list_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.knowledge_edges
    ADD CONSTRAINT knowledge_edges_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.knowledge_edges
    ADD CONSTRAINT knowledge_edges_tenant_id_from_node_id_to_node_id_predicate_key UNIQUE (tenant_id, from_node_id, to_node_id, predicate, evidence_fingerprint);
ALTER TABLE ONLY public.knowledge_evidence
    ADD CONSTRAINT knowledge_evidence_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.knowledge_extraction_jobs
    ADD CONSTRAINT knowledge_extraction_jobs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.knowledge_extraction_jobs
    ADD CONSTRAINT knowledge_extraction_jobs_tenant_id_memory_id_content_finge_key UNIQUE (tenant_id, memory_id, content_fingerprint);
ALTER TABLE ONLY public.knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_tenant_id_kind_canonical_key_key UNIQUE (tenant_id, kind, canonical_key);
ALTER TABLE ONLY public.learned_digest_log
    ADD CONSTRAINT learned_digest_log_pkey PRIMARY KEY (tenant_id, week_start);
ALTER TABLE ONLY public.marketplace_executions
    ADD CONSTRAINT marketplace_executions_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.marketplace_listings
    ADD CONSTRAINT marketplace_listings_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.marketplace_publishers
    ADD CONSTRAINT marketplace_publishers_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.marketplace_ratings
    ADD CONSTRAINT marketplace_ratings_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.marketplace_submissions
    ADD CONSTRAINT marketplace_submissions_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.marketplace_versions
    ADD CONSTRAINT marketplace_versions_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.meeting_briefs
    ADD CONSTRAINT meeting_briefs_pkey PRIMARY KEY (tenant_id, event_id);
ALTER TABLE ONLY public.memories_archive
    ADD CONSTRAINT memories_archive_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.memory_chunks
    ADD CONSTRAINT memory_chunks_pkey PRIMARY KEY (chunk_id);
ALTER TABLE ONLY public.memory_contributions
    ADD CONSTRAINT memory_contributions_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.memory_index_jobs
    ADD CONSTRAINT memory_index_jobs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.memory_index_jobs
    ADD CONSTRAINT memory_index_jobs_tenant_id_source_kind_source_id_key UNIQUE (tenant_id, source_kind, source_id);
ALTER TABLE ONLY public.memory_sync_tombstones
    ADD CONSTRAINT memory_sync_tombstones_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.mfa_challenges
    ADD CONSTRAINT mfa_challenges_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.nvidia_batch_jobs
    ADD CONSTRAINT nvidia_batch_jobs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.oauth_identities
    ADD CONSTRAINT oauth_identities_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.onboarding_state
    ADD CONSTRAINT onboarding_state_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.password_reset_tokens
    ADD CONSTRAINT password_reset_tokens_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.photo_index
    ADD CONSTRAINT photo_index_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.plugin_installs
    ADD CONSTRAINT plugin_installs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.plugins
    ADD CONSTRAINT plugins_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.prepared_local_executions
    ADD CONSTRAINT prepared_local_executions_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.proactive_push_log
    ADD CONSTRAINT proactive_push_log_pkey PRIMARY KEY (tenant_id, day);
ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.provider_failover_events
    ADD CONSTRAINT provider_failover_events_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.recap_themes
    ADD CONSTRAINT recap_themes_pkey PRIMARY KEY (user_id, period_key);
ALTER TABLE ONLY public.refresh_tokens
    ADD CONSTRAINT refresh_tokens_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.reminders
    ADD CONSTRAINT reminders_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.retrieval_leak_reports
    ADD CONSTRAINT retrieval_leak_reports_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.retrieval_telemetry_events
    ADD CONSTRAINT retrieval_telemetry_events_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.router_attempts
    ADD CONSTRAINT router_attempts_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.router_bindings
    ADD CONSTRAINT router_bindings_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.router_bindings
    ADD CONSTRAINT router_bindings_tenant_id_scope_scope_id_key UNIQUE (tenant_id, scope, scope_id);
ALTER TABLE ONLY public.router_executions
    ADD CONSTRAINT router_executions_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.router_monthly_usage
    ADD CONSTRAINT router_monthly_usage_pkey PRIMARY KEY (tenant_id, month);
ALTER TABLE ONLY public.router_outputs
    ADD CONSTRAINT router_outputs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.router_profiles
    ADD CONSTRAINT router_profiles_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.router_profiles
    ADD CONSTRAINT router_profiles_tenant_id_name_key UNIQUE (tenant_id, name);
ALTER TABLE ONLY public.router_synthesis_presets
    ADD CONSTRAINT router_synthesis_presets_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.router_synthesis_presets
    ADD CONSTRAINT router_synthesis_presets_tenant_id_name_key UNIQUE (tenant_id, name);
ALTER TABLE ONLY public.self_improvement_changes
    ADD CONSTRAINT self_improvement_changes_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.self_improvement_runs
    ADD CONSTRAINT self_improvement_runs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.self_improvement_settings
    ADD CONSTRAINT self_improvement_settings_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.self_improvement_settings
    ADD CONSTRAINT self_improvement_settings_tenant_id_key UNIQUE (tenant_id);
ALTER TABLE ONLY public.skill_run_log
    ADD CONSTRAINT skill_run_log_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.skills_state
    ADD CONSTRAINT skills_state_pkey PRIMARY KEY (tenant_id, source, name);
ALTER TABLE ONLY public.spaces
    ADD CONSTRAINT spaces_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.team_invitations
    ADD CONSTRAINT team_invitations_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.team_invitations
    ADD CONSTRAINT team_invitations_token_hash_key UNIQUE (token_hash);
ALTER TABLE ONLY public.team_memberships
    ADD CONSTRAINT team_memberships_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.team_memberships
    ADD CONSTRAINT team_memberships_team_id_user_id_key UNIQUE (team_id, user_id);
ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.agent_connections
    ADD CONSTRAINT "uq:agent_connections.token_hash" UNIQUE (token_hash);
ALTER TABLE ONLY public.agent_room_members
    ADD CONSTRAINT "uq:agent_room_members.room_id+agent_room_members.handle" UNIQUE (room_id, handle);
ALTER TABLE ONLY public.apple_reminders
    ADD CONSTRAINT "uq:apple_reminders.tenant_id+apple_reminders.external_id" UNIQUE (tenant_id, external_id);
ALTER TABLE ONLY public.billing_event_logs
    ADD CONSTRAINT "uq:billing_event_logs.event_id" UNIQUE (event_id);
ALTER TABLE ONLY public.conversation_messages
    ADD CONSTRAINT "uq:conversation_messages.local_execution_id" UNIQUE (local_execution_id);
ALTER TABLE ONLY public.device_tokens
    ADD CONSTRAINT "uq:device_tokens.token" UNIQUE (token);
ALTER TABLE ONLY public.embedding_usage
    ADD CONSTRAINT "uq:embedding_usage.tenant_id+embedding_usage.year_month" UNIQUE (tenant_id, year_month);
ALTER TABLE ONLY public.hermes_mirror_state
    ADD CONSTRAINT "uq:hermes_mirror_state.tenant_id" UNIQUE (tenant_id);
ALTER TABLE ONLY public.hermes_mirrored_jobs
    ADD CONSTRAINT "uq:hermes_mirrored_jobs.tenant_id+hermes_mirrored_jobs.hermes_j" UNIQUE (tenant_id, hermes_job_id);
ALTER TABLE ONLY public.hermes_mirrored_skills
    ADD CONSTRAINT "uq:hermes_mirrored_skills.tenant_id+hermes_mirrored_skills.name" UNIQUE (tenant_id, name);
ALTER TABLE ONLY public.hermes_profiles
    ADD CONSTRAINT "uq:hermes_profiles.hermes_profile_id" UNIQUE (hermes_profile_id);
ALTER TABLE ONLY public.hermes_profiles
    ADD CONSTRAINT "uq:hermes_profiles.tenant_id" UNIQUE (tenant_id);
ALTER TABLE ONLY public.kb_compile_reject_list
    ADD CONSTRAINT "uq:kb_compile_reject_list.tenant_id+kb_compile_reject_list.cont" UNIQUE (tenant_id, content_hash);
ALTER TABLE ONLY public.marketplace_listings
    ADD CONSTRAINT "uq:marketplace_listings.slug" UNIQUE (slug);
ALTER TABLE ONLY public.marketplace_publishers
    ADD CONSTRAINT "uq:marketplace_publishers.handle" UNIQUE (handle);
ALTER TABLE ONLY public.marketplace_publishers
    ADD CONSTRAINT "uq:marketplace_publishers.owner_user_id" UNIQUE (owner_user_id);
ALTER TABLE ONLY public.marketplace_ratings
    ADD CONSTRAINT "uq:marketplace_ratings.listing_id+marketplace_ratings.user_id" UNIQUE (listing_id, user_id);
ALTER TABLE ONLY public.marketplace_submissions
    ADD CONSTRAINT "uq:marketplace_submissions.version_id" UNIQUE (version_id);
ALTER TABLE ONLY public.marketplace_versions
    ADD CONSTRAINT "uq:marketplace_versions.listing_id+marketplace_versions.version" UNIQUE (listing_id, version);
ALTER TABLE ONLY public.memory_sync_tombstones
    ADD CONSTRAINT "uq:memory_sync_tombstones.tenant_id+memory_sync_tombstones.memo" UNIQUE (tenant_id, memory_id);
ALTER TABLE ONLY public.oauth_identities
    ADD CONSTRAINT "uq:oauth_identities.provider+oauth_identities.provider_user_id" UNIQUE (provider, provider_user_id);
ALTER TABLE ONLY public.onboarding_state
    ADD CONSTRAINT "uq:onboarding_state.tenant_id" UNIQUE (tenant_id);
ALTER TABLE ONLY public.plugin_installs
    ADD CONSTRAINT "uq:plugin_installs.tenant_id+plugin_installs.plugin_slug" UNIQUE (tenant_id, plugin_slug);
ALTER TABLE ONLY public.plugins
    ADD CONSTRAINT "uq:plugins.slug" UNIQUE (slug);
ALTER TABLE ONLY public.refresh_tokens
    ADD CONSTRAINT "uq:refresh_tokens.token_hash" UNIQUE (token_hash);
ALTER TABLE ONLY public.spaces
    ADD CONSTRAINT "uq:spaces.tenant_id+spaces.slug" UNIQUE (tenant_id, slug);
ALTER TABLE ONLY public.user_chat_preferences
    ADD CONSTRAINT "uq:user_chat_preferences.tenant_id" UNIQUE (tenant_id);
ALTER TABLE ONLY public.user_hermes_config
    ADD CONSTRAINT "uq:user_hermes_config.tenant_id" UNIQUE (tenant_id);
ALTER TABLE ONLY public.user_hermes_gateways
    ADD CONSTRAINT "uq:user_hermes_gateways.tenant_id+user_hermes_gateways.gateway_" UNIQUE (tenant_id, gateway_id);
ALTER TABLE ONLY public.user_hermes_profiles
    ADD CONSTRAINT "uq:user_hermes_profiles.tenant_id+user_hermes_profiles.slug" UNIQUE (tenant_id, slug);
ALTER TABLE ONLY public.user_llm_preferences
    ADD CONSTRAINT "uq:user_llm_preferences.tenant_id" UNIQUE (tenant_id);
ALTER TABLE ONLY public.user_provider_credentials
    ADD CONSTRAINT "uq:user_provider_credentials.tenant_id+user_provider_credential" UNIQUE (tenant_id, provider);
ALTER TABLE ONLY public.users
    ADD CONSTRAINT "uq:users.email" UNIQUE (email);
ALTER TABLE ONLY public.vault_files
    ADD CONSTRAINT "uq:vault_files.tenant_id+vault_files.path" UNIQUE (tenant_id, path);
ALTER TABLE ONLY public.webauthn_credentials
    ADD CONSTRAINT "uq:webauthn_credentials.credential_id" UNIQUE (credential_id);
ALTER TABLE ONLY public.hermes_run_events
    ADD CONSTRAINT uq_hermes_run_events_run_seq UNIQUE (run_id, seq);
ALTER TABLE ONLY public.hermes_runs
    ADD CONSTRAINT uq_hermes_runs_tenant_hermes_run_id UNIQUE (tenant_id, hermes_run_id);
ALTER TABLE ONLY public.usage_events
    ADD CONSTRAINT usage_events_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.usage_meter
    ADD CONSTRAINT usage_meter_pkey PRIMARY KEY (tenant_id, day, model);
ALTER TABLE ONLY public.user_chat_preferences
    ADD CONSTRAINT user_chat_preferences_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.user_hermes_config
    ADD CONSTRAINT user_hermes_config_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.user_hermes_gateways
    ADD CONSTRAINT user_hermes_gateways_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.user_hermes_profiles
    ADD CONSTRAINT user_hermes_profiles_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.user_llm_preferences
    ADD CONSTRAINT user_llm_preferences_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.user_provider_credential_pool_keys
    ADD CONSTRAINT user_provider_credential_pool_keys_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.user_provider_credentials
    ADD CONSTRAINT user_provider_credentials_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.vault_activity_events
    ADD CONSTRAINT vault_activity_events_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.vault_files
    ADD CONSTRAINT vault_files_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.vault_links
    ADD CONSTRAINT vault_links_pkey PRIMARY KEY (link_id);
ALTER TABLE ONLY public.vault_memberships
    ADD CONSTRAINT vault_memberships_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.vault_memberships
    ADD CONSTRAINT vault_memberships_vault_id_user_id_key UNIQUE (vault_id, user_id);
ALTER TABLE ONLY public.vaults
    ADD CONSTRAINT vaults_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.webauthn_credentials
    ADD CONSTRAINT webauthn_credentials_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.workflow_approvals
    ADD CONSTRAINT workflow_approvals_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.workflow_approvals
    ADD CONSTRAINT workflow_approvals_run_id_node_id_key UNIQUE (run_id, node_id);
ALTER TABLE ONLY public.workflow_node_runs
    ADD CONSTRAINT workflow_node_runs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.workflow_node_runs
    ADD CONSTRAINT workflow_node_runs_run_id_node_id_attempt_key UNIQUE (run_id, node_id, attempt);
ALTER TABLE ONLY public.workflow_run_events
    ADD CONSTRAINT workflow_run_events_pkey PRIMARY KEY (sequence);
ALTER TABLE ONLY public.workflow_runs
    ADD CONSTRAINT workflow_runs_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.workflow_spend_buckets
    ADD CONSTRAINT workflow_spend_buckets_pkey PRIMARY KEY (scope_key, period_kind, period_start);
ALTER TABLE ONLY public.workflow_versions
    ADD CONSTRAINT workflow_versions_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.workflow_versions
    ADD CONSTRAINT workflow_versions_workflow_id_version_key UNIQUE (workflow_id, version);
ALTER TABLE ONLY public.workflow_webhooks
    ADD CONSTRAINT workflow_webhooks_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.workflow_webhooks
    ADD CONSTRAINT workflow_webhooks_workflow_id_key UNIQUE (workflow_id);
ALTER TABLE ONLY public.workflows
    ADD CONSTRAINT workflows_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.workflows
    ADD CONSTRAINT workflows_tenant_id_name_key UNIQUE (tenant_id, name);
CREATE INDEX agent_connections_tenant_live_idx ON public.agent_connections USING btree (tenant_id) WHERE (revoked_at IS NULL);
CREATE INDEX agent_room_messages_room_idx ON public.agent_room_messages USING btree (room_id, created_at);
CREATE INDEX agent_rooms_tenant_idx ON public.agent_rooms USING btree (tenant_id, updated_at DESC);
CREATE INDEX agent_turn_traces_message_idx ON public.agent_turn_traces USING btree (conversation_message_id);
CREATE INDEX agent_turn_traces_tenant_idx ON public.agent_turn_traces USING btree (tenant_id, occurred_at DESC);
CREATE INDEX analytics_events_actor_time_idx ON public.analytics_events USING btree (actor_user_id, occurred_at DESC);
CREATE UNIQUE INDEX analytics_events_idempotency_idx ON public.analytics_events USING btree (actor_user_id, idempotency_key) WHERE (idempotency_key IS NOT NULL);
CREATE INDEX analytics_events_vault_time_idx ON public.analytics_events USING btree (vault_id, occurred_at DESC);
CREATE INDEX analytics_rollups_vault_day_idx ON public.analytics_daily_rollups USING btree (vault_id, day DESC);
CREATE INDEX apns_category_prefs_meeting_brief_idx ON public.apns_category_prefs USING btree (tenant_id) WHERE meeting_brief_enabled;
CREATE INDEX brief_feedback_tenant_created ON public.brief_feedback USING btree (tenant_id, created_at DESC);
CREATE INDEX calendar_events_tenant_starts_idx ON public.calendar_events USING btree (tenant_id, starts_at);
CREATE UNIQUE INDEX commitments_open_unique ON public.commitments USING btree (tenant_id, normalized_text) WHERE (state = 'open'::text);
CREATE INDEX commitments_tenant_state ON public.commitments USING btree (tenant_id, state, created_at DESC);
CREATE INDEX community_posts_author ON public.community_posts USING btree (author_profile_id, created_at DESC);
CREATE INDEX community_posts_feed ON public.community_posts USING btree (last_activity_at DESC, id DESC) WHERE ((parent_id IS NULL) AND (status = 'visible'::text));
CREATE INDEX community_posts_pending ON public.community_posts USING btree (created_at) WHERE (status = 'pending'::text);
CREATE INDEX community_posts_replies ON public.community_posts USING btree (parent_id, created_at, id) WHERE (parent_id IS NOT NULL);
CREATE UNIQUE INDEX community_profiles_handle_lower ON public.community_profiles USING btree (lower(handle));
CREATE INDEX community_reports_open ON public.community_reports USING btree (post_id) WHERE (status = 'open'::text);
CREATE INDEX connection_diagnostic_events_tenant_occurred_idx ON public.connection_diagnostic_events USING btree (tenant_id, occurred_at DESC);
CREATE INDEX conversation_messages_hermes_run_id_idx ON public.conversation_messages USING btree (hermes_run_id) WHERE (hermes_run_id IS NOT NULL);
CREATE INDEX conversation_messages_parallel_execution_idx ON public.conversation_messages USING btree (parallel_execution_id);
CREATE UNIQUE INDEX conversations_tenant_system_key_idx ON public.conversations USING btree (tenant_id, system_key) WHERE (system_key IS NOT NULL);
CREATE INDEX cost_ledger_tenant_day_idx ON public.cost_ledger USING btree (tenant_id, day);
CREATE INDEX device_command_queue_pending_idx ON public.device_command_queue USING btree (tenant_id, created_at) WHERE (delivered_at IS NULL);
CREATE INDEX hermes_artifacts_tenant_kind_occurred_idx ON public.hermes_artifacts USING btree (tenant_id, kind, occurred_at DESC);
CREATE INDEX hermes_artifacts_tenant_occurred_idx ON public.hermes_artifacts USING btree (tenant_id, occurred_at DESC);
CREATE UNIQUE INDEX hermes_gateway_apply_jobs_one_running_idx ON public.hermes_gateway_apply_jobs USING btree (tenant_id) WHERE (state = 'running'::text);
CREATE INDEX hermes_gateway_apply_jobs_tenant_created_idx ON public.hermes_gateway_apply_jobs USING btree (tenant_id, created_at DESC);
CREATE INDEX hermes_job_runs_tenant_job_started_idx ON public.hermes_job_runs USING btree (tenant_id, hermes_job_id, started_at DESC);
CREATE INDEX hermes_job_runs_tenant_started_idx ON public.hermes_job_runs USING btree (tenant_id, started_at DESC);
CREATE INDEX idx_achievement_progress_tenant_unlocked ON public.achievement_progress USING btree (tenant_id, unlocked_at DESC) WHERE (unlocked_at IS NOT NULL);
CREATE INDEX idx_apple_reminders_tenant_due ON public.apple_reminders USING btree (tenant_id, due_at);
CREATE INDEX idx_conversation_messages_conv_created ON public.conversation_messages USING btree (conversation_id, created_at);
CREATE INDEX idx_conversations_tenant_updated ON public.conversations USING btree (tenant_id, updated_at DESC);
CREATE INDEX idx_email_verification_tokens_tenant ON public.email_verification_tokens USING btree (tenant_id);
CREATE INDEX idx_health_events_tenant_recorded ON public.health_events USING btree (tenant_id, recorded_at DESC);
CREATE INDEX idx_health_events_tenant_type_recorded ON public.health_events USING btree (tenant_id, event_type, recorded_at DESC);
CREATE INDEX idx_hermes_run_events_run_seq ON public.hermes_run_events USING btree (run_id, seq);
CREATE INDEX idx_hermes_runs_active ON public.hermes_runs USING btree (status, started_at) WHERE (status = ANY (ARRAY['queued'::text, 'running'::text, 'waiting_for_approval'::text]));
CREATE INDEX idx_hermes_runs_tenant_started_at ON public.hermes_runs USING btree (tenant_id, started_at DESC);
CREATE INDEX idx_hermes_tenant_containers_last_used_at ON public.hermes_tenant_containers USING btree (last_used_at) WHERE (xai_connected_at IS NULL);
CREATE UNIQUE INDEX idx_hermes_tenant_containers_port ON public.hermes_tenant_containers USING btree (port);
CREATE INDEX idx_idempotency_keys_expires_at ON public.idempotency_keys USING btree (expires_at);
CREATE UNIQUE INDEX idx_idempotency_keys_tenant_key ON public.idempotency_keys USING btree (tenant_id, key);
CREATE INDEX idx_import_items_session ON public.import_items USING btree (session_id);
CREATE INDEX idx_insights_active ON public.insights USING btree (tenant_id, section, created_at DESC) WHERE (dismissed_at IS NULL);
CREATE UNIQUE INDEX idx_insights_knowledge_edge ON public.insights USING btree (knowledge_edge_id) WHERE (knowledge_edge_id IS NOT NULL);
CREATE INDEX idx_knowledge_edges_active_from ON public.knowledge_edges USING btree (tenant_id, from_node_id, confidence DESC) WHERE (state = ANY (ARRAY['asserted'::text, 'suggested'::text, 'confirmed'::text]));
CREATE INDEX idx_knowledge_edges_active_to ON public.knowledge_edges USING btree (tenant_id, to_node_id, confidence DESC) WHERE (state = ANY (ARRAY['asserted'::text, 'suggested'::text, 'confirmed'::text]));
CREATE INDEX idx_knowledge_edges_from ON public.knowledge_edges USING btree (tenant_id, from_node_id, state, confidence DESC);
CREATE INDEX idx_knowledge_edges_to ON public.knowledge_edges USING btree (tenant_id, to_node_id, state, confidence DESC);
CREATE INDEX idx_knowledge_evidence_edge ON public.knowledge_evidence USING btree (tenant_id, edge_id) WHERE (edge_id IS NOT NULL);
CREATE INDEX idx_knowledge_evidence_memory ON public.knowledge_evidence USING btree (tenant_id, memory_id);
CREATE INDEX idx_knowledge_evidence_node ON public.knowledge_evidence USING btree (tenant_id, node_id) WHERE (node_id IS NOT NULL);
CREATE INDEX idx_knowledge_jobs_claim ON public.knowledge_extraction_jobs USING btree (status, next_attempt_at, created_at);
CREATE INDEX idx_knowledge_nodes_search ON public.knowledge_nodes USING gin (to_tsvector('simple'::regconfig, ((COALESCE(label, ''::text) || ' '::text) || COALESCE(summary, ''::text))));
CREATE INDEX idx_knowledge_nodes_tenant_kind ON public.knowledge_nodes USING btree (tenant_id, kind, updated_at DESC);
CREATE INDEX idx_memories_archive_tenant_archived ON public.memories_archive USING btree (tenant_id, archived_at DESC);
CREATE INDEX idx_memories_content_tsv ON public.memories USING gin (content_tsv);
CREATE INDEX idx_memories_embedding_hnsw ON public.memories USING hnsw (embedding public.vector_cosine_ops) WITH (m='16', ef_construction='64');
CREATE INDEX idx_memories_source_vault_file ON public.memories USING btree (source_vault_file_id) WHERE (source_vault_file_id IS NOT NULL);
CREATE INDEX idx_memories_tags ON public.memories USING gin (tags);
CREATE INDEX idx_memories_tenant ON public.memories USING btree (tenant_id);
CREATE INDEX idx_memories_tenant_created ON public.memories USING btree (tenant_id, created_at DESC);
CREATE INDEX idx_memories_tenant_score ON public.memories USING btree (tenant_id, score);
CREATE INDEX idx_memories_tenant_space ON public.memories USING btree (tenant_id, space_id) WHERE (space_id IS NOT NULL);
CREATE INDEX idx_mfa_challenges_tenant ON public.mfa_challenges USING btree (tenant_id);
CREATE INDEX idx_oauth_identities_tenant ON public.oauth_identities USING btree (tenant_id);
CREATE INDEX idx_password_reset_tokens_tenant ON public.password_reset_tokens USING btree (tenant_id);
CREATE INDEX idx_photo_index_embedding_hnsw ON public.photo_index USING hnsw (embedding public.vector_cosine_ops) WITH (m='16', ef_construction='64');
CREATE UNIQUE INDEX idx_photo_index_tenant_asset ON public.photo_index USING btree (tenant_id, asset_local_id);
CREATE INDEX idx_photo_index_tenant_taken ON public.photo_index USING btree (tenant_id, taken_at DESC);
CREATE INDEX idx_projects_tenant ON public.projects USING btree (tenant_id, created_at DESC);
CREATE INDEX idx_provider_failover_provider_happened ON public.provider_failover_events USING btree (provider, happened_at DESC);
CREATE INDEX idx_provider_failover_tenant_happened ON public.provider_failover_events USING btree (tenant_id, happened_at DESC);
CREATE INDEX idx_refresh_tokens_tenant ON public.refresh_tokens USING btree (tenant_id);
CREATE INDEX idx_reminders_due ON public.reminders USING btree (tenant_id, fire_at) WHERE (fired_at IS NULL);
CREATE INDEX idx_self_improvement_changes_tenant_created ON public.self_improvement_changes USING btree (tenant_id, created_at DESC);
CREATE UNIQUE INDEX idx_self_improvement_runs_one_active ON public.self_improvement_runs USING btree (tenant_id, kind) WHERE (status = ANY (ARRAY['queued'::text, 'running'::text]));
CREATE INDEX idx_self_improvement_runs_tenant_created ON public.self_improvement_runs USING btree (tenant_id, created_at DESC);
CREATE INDEX idx_skill_run_log_tenant_started ON public.skill_run_log USING btree (tenant_id, started_at DESC);
CREATE INDEX idx_skills_state_enabled ON public.skills_state USING btree (tenant_id) WHERE enabled;
CREATE INDEX idx_spaces_tenant_category ON public.spaces USING btree (tenant_id, category);
CREATE INDEX idx_team_invitations_email ON public.team_invitations USING btree (lower(email));
CREATE INDEX idx_vault_activity_cursor ON public.vault_activity_events USING btree (vault_id, created_at DESC, id DESC);
CREATE INDEX idx_vault_files_path_trgm ON public.vault_files USING gin (path public.gin_trgm_ops);
CREATE INDEX idx_vault_files_tenant_created ON public.vault_files USING btree (tenant_id, created_at DESC);
CREATE INDEX idx_vault_files_unprocessed ON public.vault_files USING btree (tenant_id, processed_at) WHERE (processed_at IS NULL);
CREATE UNIQUE INDEX idx_vaults_personal_owner ON public.vaults USING btree (personal_owner_user_id) WHERE (personal_owner_user_id IS NOT NULL);
CREATE INDEX idx_vaults_team ON public.vaults USING btree (team_id);
CREATE INDEX ingestion_events_batch_idx ON public.ingestion_events USING btree (tenant_id, batch_id, id);
CREATE INDEX ingestion_items_batch_idx ON public.ingestion_items USING btree (tenant_id, batch_id);
CREATE INDEX ingestion_items_claim_idx ON public.ingestion_items USING btree (state, next_attempt_at, created_at) WHERE (state = 'queued'::text);
CREATE INDEX ingestion_items_dedup_idx ON public.ingestion_items USING btree (tenant_id, content_sha256, pipeline_version) WHERE ((state = 'completed'::text) AND (content_sha256 IS NOT NULL));
CREATE UNIQUE INDEX ingestion_items_source_token_idx ON public.ingestion_items USING btree (source_token_hash) WHERE (source_token_hash IS NOT NULL);
CREATE INDEX ingestion_items_state_idx ON public.ingestion_items USING btree (state, updated_at);
CREATE INDEX memories_origin_filter_idx ON public.memories USING btree (tenant_id, origin_kind, origin_provider, origin_model, created_at DESC);
CREATE UNIQUE INDEX memories_origin_source_unique ON public.memories USING btree (tenant_id, origin_kind, origin_source_id) WHERE (origin_source_id IS NOT NULL);
CREATE INDEX memories_pending_review_per_tenant ON public.memories USING btree (tenant_id) WHERE (review_state = 'pending'::text);
CREATE INDEX memories_resurface_due_idx ON public.memories USING btree (tenant_id, next_review_at) WHERE ((next_review_at IS NOT NULL) AND (NOT resurface_muted));
CREATE INDEX memories_tenant_updated_id_idx ON public.memories USING btree (tenant_id, updated_at, id);
CREATE INDEX memory_chunks_content_tsv_idx ON public.memory_chunks USING gin (content_tsv);
CREATE INDEX memory_chunks_embedding_hnsw_idx ON public.memory_chunks USING hnsw (embedding public.vector_cosine_ops) WITH (m='16', ef_construction='64');
CREATE UNIQUE INDEX memory_chunks_memory_ordinal_idx ON public.memory_chunks USING btree (memory_id, ordinal);
CREATE INDEX memory_chunks_tenant_memory_idx ON public.memory_chunks USING btree (tenant_id, memory_id);
CREATE INDEX memory_chunks_tenant_vault_file_idx ON public.memory_chunks USING btree (tenant_id, vault_file_id);
CREATE INDEX memory_contributions_filter_idx ON public.memory_contributions USING btree (tenant_id, provider, model, source_kind, created_at DESC);
CREATE INDEX memory_contributions_memory_time_idx ON public.memory_contributions USING btree (memory_id, created_at, id);
CREATE INDEX memory_index_jobs_pending_idx ON public.memory_index_jobs USING btree (status, next_attempt_at, created_at) WHERE (status = ANY (ARRAY['pending'::text, 'retry'::text]));
CREATE INDEX memory_sync_tombstones_tenant_deleted_id_idx ON public.memory_sync_tombstones USING btree (tenant_id, deleted_at, id);
CREATE INDEX nvidia_batch_jobs_tenant_created_idx ON public.nvidia_batch_jobs USING btree (tenant_id, created_at DESC);
CREATE UNIQUE INDEX retrieval_leak_reports_window_idx ON public.retrieval_leak_reports USING btree (tenant_id, period_start, period_end);
CREATE INDEX retrieval_telemetry_tenant_time_idx ON public.retrieval_telemetry_events USING btree (tenant_id, created_at DESC);
CREATE INDEX retrieval_telemetry_time_idx ON public.retrieval_telemetry_events USING btree (created_at);
CREATE INDEX router_attempts_execution_idx ON public.router_attempts USING btree (execution_id, ordinal);
CREATE INDEX router_bindings_profile_idx ON public.router_bindings USING btree (profile_id);
CREATE INDEX router_executions_tenant_time_idx ON public.router_executions USING btree (tenant_id, occurred_at DESC);
CREATE INDEX router_executions_vault_time_idx ON public.router_executions USING btree (vault_id, occurred_at DESC);
CREATE INDEX router_outputs_execution_idx ON public.router_outputs USING btree (execution_id, round, occurred_at);
CREATE INDEX router_profiles_tenant_idx ON public.router_profiles USING btree (tenant_id);
CREATE INDEX router_synthesis_presets_tenant_idx ON public.router_synthesis_presets USING btree (tenant_id, updated_at DESC);
CREATE INDEX skills_state_workflow_idx ON public.skills_state USING btree (workflow_id) WHERE (workflow_id IS NOT NULL);
CREATE UNIQUE INDEX usage_events_idempotency_idx ON public.usage_events USING btree (tenant_id, idempotency_key) WHERE (idempotency_key IS NOT NULL);
CREATE INDEX usage_events_metric_occurred_idx ON public.usage_events USING btree (metric, occurred_at DESC);
CREATE INDEX usage_events_tenant_metric_occurred_idx ON public.usage_events USING btree (tenant_id, metric, occurred_at DESC);
CREATE INDEX usage_events_tenant_occurred_idx ON public.usage_events USING btree (tenant_id, occurred_at DESC);
CREATE INDEX usage_meter_tenant_day_idx ON public.usage_meter USING btree (tenant_id, day);
CREATE UNIQUE INDEX user_hermes_profiles_one_default_per_tenant ON public.user_hermes_profiles USING btree (tenant_id) WHERE (is_default = true);
CREATE UNIQUE INDEX users_revenuecat_user_id_key ON public.users USING btree (revenuecat_user_id) WHERE (revenuecat_user_id IS NOT NULL);
CREATE INDEX users_tier_expires_idx ON public.users USING btree (tier_expires_at) WHERE (tier_expires_at IS NOT NULL);
CREATE UNIQUE INDEX users_username_key ON public.users USING btree (username);
CREATE INDEX vault_links_source_idx ON public.vault_links USING btree (tenant_id, source_vault_file_id);
CREATE INDEX vault_links_state_idx ON public.vault_links USING btree (tenant_id, resolution_state);
CREATE INDEX vault_links_target_idx ON public.vault_links USING btree (tenant_id, target_vault_file_id);
CREATE INDEX vault_links_target_slug_idx ON public.vault_links USING btree (tenant_id, target_slug);
CREATE INDEX workflow_approvals_pending_idx ON public.workflow_approvals USING btree (tenant_id, status, expires_at);
CREATE INDEX workflow_run_events_replay_idx ON public.workflow_run_events USING btree (run_id, sequence);
CREATE INDEX workflow_run_events_retention_idx ON public.workflow_run_events USING btree (created_at);
CREATE INDEX workflow_runs_claim_idx ON public.workflow_runs USING btree (status, lease_expires_at, created_at);
CREATE UNIQUE INDEX workflow_runs_dedupe_idx ON public.workflow_runs USING btree (workflow_id, dedupe_key) WHERE (dedupe_key IS NOT NULL);
CREATE INDEX workflow_runs_tenant_idx ON public.workflow_runs USING btree (tenant_id, created_at DESC);
CREATE INDEX workflow_webhooks_tenant_idx ON public.workflow_webhooks USING btree (tenant_id);
CREATE INDEX workflows_tenant_updated_idx ON public.workflows USING btree (tenant_id, updated_at DESC);
ALTER TABLE ONLY public.achievement_progress
    ADD CONSTRAINT achievement_progress_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.agent_connections
    ADD CONSTRAINT agent_connections_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.agent_room_members
    ADD CONSTRAINT agent_room_members_room_id_fkey FOREIGN KEY (room_id) REFERENCES public.agent_rooms(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.agent_room_messages
    ADD CONSTRAINT agent_room_messages_member_id_fkey FOREIGN KEY (member_id) REFERENCES public.agent_room_members(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.agent_room_messages
    ADD CONSTRAINT agent_room_messages_room_id_fkey FOREIGN KEY (room_id) REFERENCES public.agent_rooms(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.agent_turn_traces
    ADD CONSTRAINT agent_turn_traces_conversation_message_id_fkey FOREIGN KEY (conversation_message_id) REFERENCES public.conversation_messages(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.agent_turn_traces
    ADD CONSTRAINT agent_turn_traces_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.analytics_daily_rollups
    ADD CONSTRAINT analytics_daily_rollups_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.analytics_daily_rollups
    ADD CONSTRAINT analytics_daily_rollups_vault_id_fkey FOREIGN KEY (vault_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.analytics_events
    ADD CONSTRAINT analytics_events_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.analytics_events
    ADD CONSTRAINT analytics_events_vault_id_fkey FOREIGN KEY (vault_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.analytics_recommendation_states
    ADD CONSTRAINT analytics_recommendation_states_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.analytics_recommendation_states
    ADD CONSTRAINT analytics_recommendation_states_vault_id_fkey FOREIGN KEY (vault_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.apns_category_prefs
    ADD CONSTRAINT apns_category_prefs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.apple_consent
    ADD CONSTRAINT apple_consent_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.apple_reminders
    ADD CONSTRAINT apple_reminders_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.brief_feedback
    ADD CONSTRAINT brief_feedback_brief_id_fkey FOREIGN KEY (brief_id) REFERENCES public.briefs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.brief_feedback
    ADD CONSTRAINT brief_feedback_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.briefs
    ADD CONSTRAINT briefs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.calendar_accounts
    ADD CONSTRAINT calendar_accounts_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.calendar_events
    ADD CONSTRAINT calendar_events_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.commitments
    ADD CONSTRAINT commitments_memory_id_fkey FOREIGN KEY (memory_id) REFERENCES public.memories(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.commitments
    ADD CONSTRAINT commitments_source_vault_file_id_fkey FOREIGN KEY (source_vault_file_id) REFERENCES public.vault_files(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.commitments
    ADD CONSTRAINT commitments_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.community_blocks
    ADD CONSTRAINT community_blocks_blocked_profile_id_fkey FOREIGN KEY (blocked_profile_id) REFERENCES public.community_profiles(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.community_blocks
    ADD CONSTRAINT community_blocks_blocker_profile_id_fkey FOREIGN KEY (blocker_profile_id) REFERENCES public.community_profiles(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.community_daily_posts
    ADD CONSTRAINT community_daily_posts_post_id_fkey FOREIGN KEY (post_id) REFERENCES public.community_posts(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.community_posts
    ADD CONSTRAINT community_posts_author_profile_id_fkey FOREIGN KEY (author_profile_id) REFERENCES public.community_profiles(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.community_posts
    ADD CONSTRAINT community_posts_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES public.community_posts(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.community_profiles
    ADD CONSTRAINT community_profiles_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.community_reports
    ADD CONSTRAINT community_reports_post_id_fkey FOREIGN KEY (post_id) REFERENCES public.community_posts(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.community_reports
    ADD CONSTRAINT community_reports_reporter_profile_id_fkey FOREIGN KEY (reporter_profile_id) REFERENCES public.community_profiles(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.community_reports
    ADD CONSTRAINT community_reports_resolved_by_user_id_fkey FOREIGN KEY (resolved_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.connection_diagnostic_events
    ADD CONSTRAINT connection_diagnostic_events_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.conversation_messages
    ADD CONSTRAINT conversation_messages_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.conversation_messages
    ADD CONSTRAINT conversation_messages_local_execution_id_fkey FOREIGN KEY (local_execution_id) REFERENCES public.prepared_local_executions(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.conversation_messages
    ADD CONSTRAINT conversation_messages_parallel_execution_id_fkey FOREIGN KEY (parallel_execution_id) REFERENCES public.router_executions(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_space_id_fkey FOREIGN KEY (space_id) REFERENCES public.spaces(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.conversations
    ADD CONSTRAINT conversations_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.cost_ledger
    ADD CONSTRAINT cost_ledger_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.device_tokens
    ADD CONSTRAINT device_tokens_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.email_verification_tokens
    ADD CONSTRAINT email_verification_tokens_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.health_events
    ADD CONSTRAINT health_events_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.hermes_artifacts
    ADD CONSTRAINT hermes_artifacts_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.hermes_gateway_apply_jobs
    ADD CONSTRAINT hermes_gateway_apply_jobs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.hermes_job_runs
    ADD CONSTRAINT hermes_job_runs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.hermes_job_runs
    ADD CONSTRAINT hermes_job_runs_vault_file_id_fkey FOREIGN KEY (vault_file_id) REFERENCES public.vault_files(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.hermes_mirror_state
    ADD CONSTRAINT hermes_mirror_state_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.hermes_mirror_webhooks
    ADD CONSTRAINT hermes_mirror_webhooks_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.hermes_mirrored_jobs
    ADD CONSTRAINT hermes_mirrored_jobs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.hermes_mirrored_skills
    ADD CONSTRAINT hermes_mirrored_skills_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.hermes_profiles
    ADD CONSTRAINT hermes_profiles_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.hermes_run_events
    ADD CONSTRAINT hermes_run_events_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.hermes_runs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.hermes_runs
    ADD CONSTRAINT hermes_runs_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.hermes_runs
    ADD CONSTRAINT hermes_runs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.hermes_tenant_containers
    ADD CONSTRAINT hermes_tenant_containers_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.import_items
    ADD CONSTRAINT import_items_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.import_sessions(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.import_items
    ADD CONSTRAINT import_items_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.import_sessions
    ADD CONSTRAINT import_sessions_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.ingestion_batches
    ADD CONSTRAINT ingestion_batches_space_id_fkey FOREIGN KEY (space_id) REFERENCES public.spaces(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.ingestion_batches
    ADD CONSTRAINT ingestion_batches_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.ingestion_events
    ADD CONSTRAINT ingestion_events_batch_id_fkey FOREIGN KEY (batch_id) REFERENCES public.ingestion_batches(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.ingestion_events
    ADD CONSTRAINT ingestion_events_item_id_fkey FOREIGN KEY (item_id) REFERENCES public.ingestion_items(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.ingestion_events
    ADD CONSTRAINT ingestion_events_tenant_vault_fk FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.ingestion_items
    ADD CONSTRAINT ingestion_items_batch_id_fkey FOREIGN KEY (batch_id) REFERENCES public.ingestion_batches(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.ingestion_items
    ADD CONSTRAINT ingestion_items_memory_id_fkey FOREIGN KEY (memory_id) REFERENCES public.memories(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.ingestion_items
    ADD CONSTRAINT ingestion_items_reused_from_item_id_fkey FOREIGN KEY (reused_from_item_id) REFERENCES public.ingestion_items(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.ingestion_items
    ADD CONSTRAINT ingestion_items_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.ingestion_items
    ADD CONSTRAINT ingestion_items_vault_file_id_fkey FOREIGN KEY (vault_file_id) REFERENCES public.vault_files(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.insights
    ADD CONSTRAINT insights_knowledge_edge_id_fkey FOREIGN KEY (knowledge_edge_id) REFERENCES public.knowledge_edges(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.insights
    ADD CONSTRAINT insights_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.kanban_boards
    ADD CONSTRAINT kanban_boards_created_by_user_id_fkey FOREIGN KEY (created_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.kanban_boards
    ADD CONSTRAINT kanban_boards_tenant_vault_fk FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.kanban_boards
    ADD CONSTRAINT kanban_boards_updated_by_user_id_fkey FOREIGN KEY (updated_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.kanban_cards
    ADD CONSTRAINT kanban_cards_board_id_fkey FOREIGN KEY (board_id) REFERENCES public.kanban_boards(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.kanban_cards
    ADD CONSTRAINT kanban_cards_column_id_fkey FOREIGN KEY (column_id) REFERENCES public.kanban_columns(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.kanban_cards
    ADD CONSTRAINT kanban_cards_created_by_user_id_fkey FOREIGN KEY (created_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.kanban_cards
    ADD CONSTRAINT kanban_cards_tenant_vault_fk FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.kanban_cards
    ADD CONSTRAINT kanban_cards_updated_by_user_id_fkey FOREIGN KEY (updated_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.kanban_columns
    ADD CONSTRAINT kanban_columns_board_id_fkey FOREIGN KEY (board_id) REFERENCES public.kanban_boards(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.kanban_columns
    ADD CONSTRAINT kanban_columns_tenant_vault_fk FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.kb_compile_reject_list
    ADD CONSTRAINT kb_compile_reject_list_tenant_vault_fk FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.knowledge_edges
    ADD CONSTRAINT knowledge_edges_from_node_id_fkey FOREIGN KEY (from_node_id) REFERENCES public.knowledge_nodes(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.knowledge_edges
    ADD CONSTRAINT knowledge_edges_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.knowledge_edges
    ADD CONSTRAINT knowledge_edges_to_node_id_fkey FOREIGN KEY (to_node_id) REFERENCES public.knowledge_nodes(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.knowledge_evidence
    ADD CONSTRAINT knowledge_evidence_edge_id_fkey FOREIGN KEY (edge_id) REFERENCES public.knowledge_edges(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.knowledge_evidence
    ADD CONSTRAINT knowledge_evidence_memory_id_fkey FOREIGN KEY (memory_id) REFERENCES public.memories(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.knowledge_evidence
    ADD CONSTRAINT knowledge_evidence_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.knowledge_nodes(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.knowledge_evidence
    ADD CONSTRAINT knowledge_evidence_source_vault_file_id_fkey FOREIGN KEY (source_vault_file_id) REFERENCES public.vault_files(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.knowledge_evidence
    ADD CONSTRAINT knowledge_evidence_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.knowledge_extraction_jobs
    ADD CONSTRAINT knowledge_extraction_jobs_memory_id_fkey FOREIGN KEY (memory_id) REFERENCES public.memories(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.knowledge_extraction_jobs
    ADD CONSTRAINT knowledge_extraction_jobs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.knowledge_nodes
    ADD CONSTRAINT knowledge_nodes_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.learned_digest_log
    ADD CONSTRAINT learned_digest_log_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.marketplace_executions
    ADD CONSTRAINT marketplace_executions_install_id_fkey FOREIGN KEY (install_id) REFERENCES public.plugin_installs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.marketplace_executions
    ADD CONSTRAINT marketplace_executions_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.marketplace_executions
    ADD CONSTRAINT marketplace_executions_version_id_fkey FOREIGN KEY (version_id) REFERENCES public.marketplace_versions(id) ON DELETE RESTRICT;
ALTER TABLE ONLY public.marketplace_listings
    ADD CONSTRAINT marketplace_listings_publisher_id_fkey FOREIGN KEY (publisher_id) REFERENCES public.marketplace_publishers(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.marketplace_publishers
    ADD CONSTRAINT marketplace_publishers_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.marketplace_ratings
    ADD CONSTRAINT marketplace_ratings_listing_id_fkey FOREIGN KEY (listing_id) REFERENCES public.marketplace_listings(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.marketplace_ratings
    ADD CONSTRAINT marketplace_ratings_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.marketplace_submissions
    ADD CONSTRAINT marketplace_submissions_publisher_user_id_fkey FOREIGN KEY (publisher_user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.marketplace_submissions
    ADD CONSTRAINT marketplace_submissions_reviewed_by_user_id_fkey FOREIGN KEY (reviewed_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.marketplace_submissions
    ADD CONSTRAINT marketplace_submissions_version_id_fkey FOREIGN KEY (version_id) REFERENCES public.marketplace_versions(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.marketplace_versions
    ADD CONSTRAINT marketplace_versions_listing_id_fkey FOREIGN KEY (listing_id) REFERENCES public.marketplace_listings(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.meeting_briefs
    ADD CONSTRAINT meeting_briefs_event_id_fkey FOREIGN KEY (event_id) REFERENCES public.calendar_events(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.meeting_briefs
    ADD CONSTRAINT meeting_briefs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.memories_archive
    ADD CONSTRAINT memories_archive_tenant_vault_fk FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_created_by_user_id_fkey FOREIGN KEY (created_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_origin_conversation_message_id_fkey FOREIGN KEY (origin_conversation_message_id) REFERENCES public.conversation_messages(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_source_vault_file_id_fkey FOREIGN KEY (source_vault_file_id) REFERENCES public.vault_files(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_space_id_fkey FOREIGN KEY (space_id) REFERENCES public.spaces(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_tenant_vault_fk FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.memories
    ADD CONSTRAINT memories_updated_by_user_id_fkey FOREIGN KEY (updated_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.memory_chunks
    ADD CONSTRAINT memory_chunks_memory_id_fkey FOREIGN KEY (memory_id) REFERENCES public.memories(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.memory_chunks
    ADD CONSTRAINT memory_chunks_vault_file_id_fkey FOREIGN KEY (vault_file_id) REFERENCES public.vault_files(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.memory_contributions
    ADD CONSTRAINT memory_contributions_memory_id_fkey FOREIGN KEY (memory_id) REFERENCES public.memories(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.memory_contributions
    ADD CONSTRAINT memory_contributions_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.memory_index_jobs
    ADD CONSTRAINT memory_index_jobs_conversation_message_id_fkey FOREIGN KEY (conversation_message_id) REFERENCES public.conversation_messages(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.memory_index_jobs
    ADD CONSTRAINT memory_index_jobs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.memory_sync_tombstones
    ADD CONSTRAINT memory_sync_tombstones_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.mfa_challenges
    ADD CONSTRAINT mfa_challenges_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.nvidia_batch_jobs
    ADD CONSTRAINT nvidia_batch_jobs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.oauth_identities
    ADD CONSTRAINT oauth_identities_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.onboarding_state
    ADD CONSTRAINT onboarding_state_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.password_reset_tokens
    ADD CONSTRAINT password_reset_tokens_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.photo_index
    ADD CONSTRAINT photo_index_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.plugin_installs
    ADD CONSTRAINT plugin_installs_marketplace_version_id_fkey FOREIGN KEY (marketplace_version_id) REFERENCES public.marketplace_versions(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.plugin_installs
    ADD CONSTRAINT plugin_installs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.prepared_local_executions
    ADD CONSTRAINT prepared_local_executions_committed_message_id_fkey FOREIGN KEY (committed_message_id) REFERENCES public.conversation_messages(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.prepared_local_executions
    ADD CONSTRAINT prepared_local_executions_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.prepared_local_executions
    ADD CONSTRAINT prepared_local_executions_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.prepared_local_executions
    ADD CONSTRAINT prepared_local_executions_user_message_id_fkey FOREIGN KEY (user_message_id) REFERENCES public.conversation_messages(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.proactive_push_log
    ADD CONSTRAINT proactive_push_log_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.provider_failover_events
    ADD CONSTRAINT provider_failover_events_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.recap_themes
    ADD CONSTRAINT recap_themes_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.refresh_tokens
    ADD CONSTRAINT refresh_tokens_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.reminders
    ADD CONSTRAINT reminders_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.router_attempts
    ADD CONSTRAINT router_attempts_execution_id_fkey FOREIGN KEY (execution_id) REFERENCES public.router_executions(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.router_bindings
    ADD CONSTRAINT router_bindings_profile_id_fkey FOREIGN KEY (profile_id) REFERENCES public.router_profiles(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.router_bindings
    ADD CONSTRAINT router_bindings_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.router_executions
    ADD CONSTRAINT router_executions_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.router_executions
    ADD CONSTRAINT router_executions_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.conversations(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.router_executions
    ADD CONSTRAINT router_executions_profile_id_fkey FOREIGN KEY (profile_id) REFERENCES public.router_profiles(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.router_executions
    ADD CONSTRAINT router_executions_space_id_fkey FOREIGN KEY (space_id) REFERENCES public.spaces(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.router_executions
    ADD CONSTRAINT router_executions_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.router_executions
    ADD CONSTRAINT router_executions_vault_id_fkey FOREIGN KEY (vault_id) REFERENCES public.vaults(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.router_monthly_usage
    ADD CONSTRAINT router_monthly_usage_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.router_outputs
    ADD CONSTRAINT router_outputs_execution_id_fkey FOREIGN KEY (execution_id) REFERENCES public.router_executions(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.router_profiles
    ADD CONSTRAINT router_profiles_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.router_synthesis_presets
    ADD CONSTRAINT router_synthesis_presets_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.self_improvement_changes
    ADD CONSTRAINT self_improvement_changes_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.self_improvement_runs(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.self_improvement_changes
    ADD CONSTRAINT self_improvement_changes_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.self_improvement_runs
    ADD CONSTRAINT self_improvement_runs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.self_improvement_settings
    ADD CONSTRAINT self_improvement_settings_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.skill_run_log
    ADD CONSTRAINT skill_run_log_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.skills_state
    ADD CONSTRAINT skills_state_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.skills_state
    ADD CONSTRAINT skills_state_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.spaces
    ADD CONSTRAINT spaces_created_by_user_id_fkey FOREIGN KEY (created_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.spaces
    ADD CONSTRAINT spaces_tenant_vault_fk FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.spaces
    ADD CONSTRAINT spaces_updated_by_user_id_fkey FOREIGN KEY (updated_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.team_invitations
    ADD CONSTRAINT team_invitations_invited_by_user_id_fkey FOREIGN KEY (invited_by_user_id) REFERENCES public.users(id) ON DELETE RESTRICT;
ALTER TABLE ONLY public.team_invitations
    ADD CONSTRAINT team_invitations_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.team_memberships
    ADD CONSTRAINT team_memberships_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.team_memberships
    ADD CONSTRAINT team_memberships_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_billing_sponsor_user_id_fkey FOREIGN KEY (billing_sponsor_user_id) REFERENCES public.users(id) ON DELETE RESTRICT;
ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES public.users(id) ON DELETE RESTRICT;
ALTER TABLE ONLY public.usage_events
    ADD CONSTRAINT usage_events_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.usage_meter
    ADD CONSTRAINT usage_meter_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.user_chat_preferences
    ADD CONSTRAINT user_chat_preferences_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.user_hermes_config
    ADD CONSTRAINT user_hermes_config_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.user_hermes_gateways
    ADD CONSTRAINT user_hermes_gateways_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.user_hermes_profiles
    ADD CONSTRAINT user_hermes_profiles_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.user_llm_preferences
    ADD CONSTRAINT user_llm_preferences_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.user_provider_credential_pool_keys
    ADD CONSTRAINT user_provider_credential_pool_keys_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.user_provider_credentials
    ADD CONSTRAINT user_provider_credentials_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.vault_activity_events
    ADD CONSTRAINT vault_activity_events_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.vault_activity_events
    ADD CONSTRAINT vault_activity_events_vault_id_fkey FOREIGN KEY (vault_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.vault_files
    ADD CONSTRAINT vault_files_created_by_user_id_fkey FOREIGN KEY (created_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.vault_files
    ADD CONSTRAINT vault_files_space_id_fkey FOREIGN KEY (space_id) REFERENCES public.spaces(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.vault_files
    ADD CONSTRAINT vault_files_tenant_vault_fk FOREIGN KEY (tenant_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.vault_files
    ADD CONSTRAINT vault_files_updated_by_user_id_fkey FOREIGN KEY (updated_by_user_id) REFERENCES public.users(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.vault_links
    ADD CONSTRAINT vault_links_source_vault_file_id_fkey FOREIGN KEY (source_vault_file_id) REFERENCES public.vault_files(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.vault_links
    ADD CONSTRAINT vault_links_target_vault_file_id_fkey FOREIGN KEY (target_vault_file_id) REFERENCES public.vault_files(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.vault_memberships
    ADD CONSTRAINT vault_memberships_created_by_user_id_fkey FOREIGN KEY (created_by_user_id) REFERENCES public.users(id) ON DELETE RESTRICT;
ALTER TABLE ONLY public.vault_memberships
    ADD CONSTRAINT vault_memberships_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.vault_memberships
    ADD CONSTRAINT vault_memberships_vault_id_fkey FOREIGN KEY (vault_id) REFERENCES public.vaults(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.vaults
    ADD CONSTRAINT vaults_personal_owner_user_id_fkey FOREIGN KEY (personal_owner_user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.vaults
    ADD CONSTRAINT vaults_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.webauthn_credentials
    ADD CONSTRAINT webauthn_credentials_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflow_approvals
    ADD CONSTRAINT workflow_approvals_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.workflow_runs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflow_approvals
    ADD CONSTRAINT workflow_approvals_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflow_approvals
    ADD CONSTRAINT workflow_approvals_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflow_node_runs
    ADD CONSTRAINT workflow_node_runs_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.workflow_runs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflow_run_events
    ADD CONSTRAINT workflow_run_events_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.workflow_runs(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflow_run_events
    ADD CONSTRAINT workflow_run_events_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflow_runs
    ADD CONSTRAINT workflow_runs_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflow_runs
    ADD CONSTRAINT workflow_runs_version_id_fkey FOREIGN KEY (version_id) REFERENCES public.workflow_versions(id) ON DELETE RESTRICT;
ALTER TABLE ONLY public.workflow_runs
    ADD CONSTRAINT workflow_runs_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflow_versions
    ADD CONSTRAINT workflow_versions_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflow_versions
    ADD CONSTRAINT workflow_versions_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflow_webhooks
    ADD CONSTRAINT workflow_webhooks_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflow_webhooks
    ADD CONSTRAINT workflow_webhooks_workflow_id_fkey FOREIGN KEY (workflow_id) REFERENCES public.workflows(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.workflows
    ADD CONSTRAINT workflows_published_version_fk FOREIGN KEY (published_version_id) REFERENCES public.workflow_versions(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.workflows
    ADD CONSTRAINT workflows_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.users(id) ON DELETE CASCADE;
