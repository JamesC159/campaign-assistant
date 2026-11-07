-- Core schema: accounts, campaigns, game sessions, canon, proposals, jobs.
--
-- Security model
-- --------------
-- The API and worker run as role campaign_app (NOBYPASSRLS). Every request
-- runs in a transaction that sets app.user_id; row-level security policies on
-- campaign data decide what that user may see. Players can only ever read
-- party-visible, approved canon: this is enforced here, not in Go.
--
-- Account tables (users, auth_sessions, magic_links) and jobs are not under
-- RLS: they are only reached through the auth and job packages, before a user
-- context exists.

-- Roles are cluster-wide, so several databases (e.g. parallel test databases)
-- may race to create it; whoever loses the race just uses the existing role.
DO $$
BEGIN
  CREATE ROLE campaign_app NOLOGIN NOSUPERUSER NOBYPASSRLS;
EXCEPTION WHEN duplicate_object OR unique_violation THEN
  NULL;
END
$$;

-- The current request's user, or NULL outside a user context.
CREATE FUNCTION app_user_id() RETURNS uuid
LANGUAGE sql STABLE AS $$
  SELECT NULLIF(current_setting('app.user_id', true), '')::uuid
$$;

-- ---------------------------------------------------------------- accounts

CREATE TABLE users (
  id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  email         citext      NOT NULL UNIQUE,
  password_hash text,                       -- NULL for magic-link-only accounts
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE auth_sessions (
  id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      uuid        NOT NULL REFERENCES users ON DELETE CASCADE,
  token_hash   bytea       NOT NULL UNIQUE, -- sha256 of the cookie token; the token itself is never stored
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  last_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX auth_sessions_user_idx ON auth_sessions (user_id);

CREATE TABLE magic_links (
  id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    uuid        NOT NULL REFERENCES users ON DELETE CASCADE,
  token_hash bytea       NOT NULL UNIQUE,
  expires_at timestamptz NOT NULL,
  used_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- --------------------------------------------------------------- campaigns

CREATE TABLE campaigns (
  id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id   uuid        NOT NULL REFERENCES users ON DELETE CASCADE,
  name       text        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE campaign_members (
  campaign_id uuid        NOT NULL REFERENCES campaigns ON DELETE CASCADE,
  user_id     uuid        NOT NULL REFERENCES users ON DELETE CASCADE,
  role        text        NOT NULL CHECK (role IN ('gm', 'player')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (campaign_id, user_id)
);
CREATE INDEX campaign_members_user_idx ON campaign_members (user_id);

-- Membership checks used by policies. SECURITY DEFINER lets them read
-- campaign_members without recursing into its own policies.
CREATE FUNCTION is_member(c uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT EXISTS (SELECT 1 FROM campaign_members WHERE campaign_id = c AND user_id = app_user_id())
$$;

CREATE FUNCTION is_gm(c uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT EXISTS (SELECT 1 FROM campaign_members WHERE campaign_id = c AND user_id = app_user_id() AND role = 'gm')
$$;

CREATE FUNCTION is_owner(c uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT EXISTS (SELECT 1 FROM campaigns WHERE id = c AND owner_id = app_user_id())
$$;

-- ----------------------------------------------------------- game sessions

CREATE TABLE game_sessions (
  id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  campaign_id uuid        NOT NULL REFERENCES campaigns ON DELETE CASCADE,
  number      integer     NOT NULL CHECK (number > 0),
  played_on   date,
  notes       text        NOT NULL,         -- raw GM notes, including secrets: GM-only
  status      text        NOT NULL DEFAULT 'queued'
                          CHECK (status IN ('queued', 'processing', 'review', 'approved', 'failed')),
  error       text,
  created_by  uuid        NOT NULL REFERENCES users,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (campaign_id, number)
);

-- A proposal is the pipeline's full output for one session (entities, facts,
-- contradictions, review items). It is GM-only until approved into canon.
CREATE TABLE proposals (
  session_id    uuid        PRIMARY KEY REFERENCES game_sessions ON DELETE CASCADE,
  campaign_id   uuid        NOT NULL REFERENCES campaigns ON DELETE CASCADE,
  body          jsonb       NOT NULL,
  -- Number of approved sessions when this proposal was computed. If canon has
  -- changed since, the proposal is stale and must be recomputed before approval.
  canon_version integer     NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE recaps (
  session_id  uuid        NOT NULL REFERENCES game_sessions ON DELETE CASCADE,
  campaign_id uuid        NOT NULL REFERENCES campaigns ON DELETE CASCADE,
  -- Copied from game_sessions so players, who can't read game_sessions, can order recaps.
  session_number integer  NOT NULL,
  audience    text        NOT NULL CHECK (audience IN ('gm', 'party')),
  body        text        NOT NULL,
  published   boolean     NOT NULL DEFAULT false, -- party recaps become visible when the session is approved
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, audience)
);

-- ------------------------------------------------------------------- canon

CREATE TABLE entities (
  id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  campaign_id uuid        NOT NULL REFERENCES campaigns ON DELETE CASCADE,
  type        text        NOT NULL CHECK (type IN ('npc', 'pc', 'place', 'faction', 'item', 'thread')),
  description text        NOT NULL DEFAULT '', -- may contain secrets: GM-only column, see entity_party view
  first_seen  integer     NOT NULL DEFAULT 0,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX entities_campaign_idx ON entities (campaign_id);

-- Names live in their own table so each name has its own visibility:
-- players may know "the Hooded Stranger" without knowing her real name.
CREATE TABLE entity_names (
  entity_id   uuid        NOT NULL REFERENCES entities ON DELETE CASCADE,
  campaign_id uuid        NOT NULL REFERENCES campaigns ON DELETE CASCADE,
  name        text        NOT NULL,
  position    integer     NOT NULL,           -- 0 = canonical (GM-facing) name
  visibility  text        NOT NULL CHECK (visibility IN ('gm', 'party')),
  PRIMARY KEY (entity_id, position)
);
CREATE UNIQUE INDEX entity_names_unique_idx ON entity_names (entity_id, lower(name));

CREATE TABLE facts (
  id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  campaign_id    uuid        NOT NULL REFERENCES campaigns ON DELETE CASCADE,
  entity_id      uuid        NOT NULL REFERENCES entities ON DELETE CASCADE,
  session_id     uuid        NOT NULL REFERENCES game_sessions ON DELETE CASCADE,
  session_number integer     NOT NULL,
  statement      text        NOT NULL,
  quote          text        NOT NULL,
  visibility     text        NOT NULL CHECK (visibility IN ('gm', 'party')),
  state          text        NOT NULL DEFAULT 'canon' CHECK (state IN ('canon', 'retconned')),
  prompt_version integer     NOT NULL,
  embedding      vector(1024),
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX facts_entity_idx ON facts (entity_id);
CREATE INDEX facts_campaign_idx ON facts (campaign_id);

-- Does the party know this entity by at least one name?
CREATE FUNCTION entity_known_to_party(e uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT EXISTS (SELECT 1 FROM entity_names WHERE entity_id = e AND visibility = 'party')
$$;

-- ------------------------------------------------------------------ usage

CREATE TABLE usage_events (
  id            bigserial   PRIMARY KEY,
  campaign_id   uuid        NOT NULL REFERENCES campaigns ON DELETE CASCADE,
  user_id       uuid        NOT NULL REFERENCES users ON DELETE CASCADE,
  session_id    uuid        REFERENCES game_sessions ON DELETE SET NULL,
  purpose       text        NOT NULL,
  calls         integer     NOT NULL,
  input_tokens  integer     NOT NULL,
  output_tokens integer     NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX usage_events_user_idx ON usage_events (user_id, created_at);

-- ------------------------------------------------------------------- jobs

CREATE TABLE jobs (
  id           bigserial   PRIMARY KEY,
  kind         text        NOT NULL,
  payload      jsonb       NOT NULL DEFAULT '{}',
  run_at       timestamptz NOT NULL DEFAULT now(),
  attempts     integer     NOT NULL DEFAULT 0,
  max_attempts integer     NOT NULL DEFAULT 5,
  locked_until timestamptz,
  locked_by    text,
  last_error   text,
  done_at      timestamptz,
  failed_at    timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now()
);
-- Only runnable jobs are indexed, so the claim query stays fast as history grows.
CREATE INDEX jobs_runnable_idx ON jobs (run_at) WHERE done_at IS NULL AND failed_at IS NULL;

-- ------------------------------------------------------------ RLS policies

ALTER TABLE campaigns        ENABLE ROW LEVEL SECURITY;
ALTER TABLE campaign_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE game_sessions    ENABLE ROW LEVEL SECURITY;
ALTER TABLE proposals        ENABLE ROW LEVEL SECURITY;
ALTER TABLE recaps           ENABLE ROW LEVEL SECURITY;
ALTER TABLE entities         ENABLE ROW LEVEL SECURITY;
ALTER TABLE entity_names     ENABLE ROW LEVEL SECURITY;
ALTER TABLE facts            ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_events     ENABLE ROW LEVEL SECURITY;

-- campaigns: members read; the creator inserts; GMs update; only the owner deletes.
CREATE POLICY campaigns_select ON campaigns FOR SELECT USING (is_member(id) OR owner_id = app_user_id());
CREATE POLICY campaigns_insert ON campaigns FOR INSERT WITH CHECK (owner_id = app_user_id());
CREATE POLICY campaigns_update ON campaigns FOR UPDATE USING (is_gm(id)) WITH CHECK (is_gm(id));
CREATE POLICY campaigns_delete ON campaigns FOR DELETE USING (is_owner(id));

-- members: members see the roster; GMs manage it; the owner may add themselves as GM at creation.
-- Everyone can see their own membership row; INSERT ... ON CONFLICT also checks
-- the proposed row against SELECT, so this is needed when the owner adds themselves.
CREATE POLICY members_select ON campaign_members FOR SELECT USING (is_member(campaign_id) OR user_id = app_user_id());
CREATE POLICY members_update ON campaign_members FOR UPDATE USING (is_gm(campaign_id)) WITH CHECK (is_gm(campaign_id));
CREATE POLICY members_insert ON campaign_members FOR INSERT WITH CHECK (
  is_gm(campaign_id) OR (user_id = app_user_id() AND role = 'gm' AND is_owner(campaign_id))
);
CREATE POLICY members_delete ON campaign_members FOR DELETE USING (is_gm(campaign_id) AND NOT (user_id = app_user_id() AND is_owner(campaign_id)));

-- Raw notes and proposals contain secrets: GM only, for every operation.
CREATE POLICY sessions_gm  ON game_sessions FOR ALL USING (is_gm(campaign_id)) WITH CHECK (is_gm(campaign_id));
CREATE POLICY proposals_gm ON proposals     FOR ALL USING (is_gm(campaign_id)) WITH CHECK (is_gm(campaign_id));
CREATE POLICY usage_gm     ON usage_events  FOR ALL USING (is_gm(campaign_id)) WITH CHECK (is_gm(campaign_id));

-- Recaps: GMs see all; players see published party recaps.
CREATE POLICY recaps_gm    ON recaps FOR ALL    USING (is_gm(campaign_id)) WITH CHECK (is_gm(campaign_id));
CREATE POLICY recaps_party ON recaps FOR SELECT USING (audience = 'party' AND published AND is_member(campaign_id));

-- Canon: GMs see and write everything. Players see only what the party knows.
CREATE POLICY entities_gm    ON entities FOR ALL    USING (is_gm(campaign_id)) WITH CHECK (is_gm(campaign_id));
CREATE POLICY entities_party ON entities FOR SELECT USING (is_member(campaign_id) AND entity_known_to_party(id));

CREATE POLICY names_gm    ON entity_names FOR ALL    USING (is_gm(campaign_id)) WITH CHECK (is_gm(campaign_id));
CREATE POLICY names_party ON entity_names FOR SELECT USING (is_member(campaign_id) AND visibility = 'party');

CREATE POLICY facts_gm    ON facts FOR ALL    USING (is_gm(campaign_id)) WITH CHECK (is_gm(campaign_id));
CREATE POLICY facts_party ON facts FOR SELECT USING (
  is_member(campaign_id) AND visibility = 'party' AND state = 'canon' AND entity_known_to_party(entity_id)
);

-- entities.description may hold secrets. Players can read entity rows (for
-- type, first_seen), so revoke the column itself from the app role and expose
-- it only through this GM-gated view.
CREATE VIEW entity_descriptions WITH (security_barrier) AS
  SELECT id, campaign_id, description FROM entities WHERE is_gm(campaign_id);

-- -------------------------------------------------------------- privileges

GRANT USAGE ON SCHEMA public TO campaign_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON users, auth_sessions, magic_links, jobs TO campaign_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON campaigns, campaign_members, game_sessions, proposals, recaps,
  entity_names, facts, usage_events TO campaign_app;
GRANT SELECT (id, campaign_id, type, first_seen, created_at), INSERT, UPDATE, DELETE ON entities TO campaign_app;
GRANT SELECT ON entity_descriptions TO campaign_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO campaign_app;
GRANT EXECUTE ON FUNCTION app_user_id(), is_member(uuid), is_gm(uuid), is_owner(uuid), entity_known_to_party(uuid) TO campaign_app;
