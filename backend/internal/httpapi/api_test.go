package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"campaign-assistant/backend/internal/auth"
	"campaign-assistant/backend/internal/canon"
	"campaign-assistant/backend/internal/config"
	"campaign-assistant/backend/internal/embed"
	"campaign-assistant/backend/internal/httpapi"
	"campaign-assistant/backend/internal/jobs"
	"campaign-assistant/backend/internal/mail"
	"campaign-assistant/backend/internal/notify"
	"campaign-assistant/backend/internal/processing"
	"campaign-assistant/backend/internal/store"
	"campaign-assistant/backend/internal/testdb"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	worker *jobs.Worker
	box    *mail.Mailbox
}

func newEnv(t *testing.T) *env {
	d := testdb.New(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(d.App)
	box := &mail.Mailbox{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	broker := &notify.Broker{Pool: d.App, Log: log}
	go broker.Run(ctx)

	h := httpapi.NewHandler(httpapi.Deps{
		Log: log, DB: d.App, Env: config.EnvDev, Provider: config.ProviderMock, Version: "test",
		Store: st, Events: broker, Mailbox: box,
		Auth: &auth.Service{Store: st, Mail: box, Log: log, Params: auth.TestParams, AppURL: "http://app.test"},
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	proc := &processing.Processor{Store: st, Engine: &canon.Engine{Model: canon.HeuristicModel{}, Embedder: embed.Hash{}}, Log: log}
	w := &jobs.Worker{Store: st, Log: log, Handlers: map[string]jobs.Handler{processing.Kind: proc.Handle}, OnFailed: proc.Failed}
	return &env{t: t, srv: srv, worker: w, box: box}
}

// drain runs queued jobs until none are left.
func (e *env) drain() {
	e.t.Helper()
	for {
		ran, err := e.worker.RunOne(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		if !ran {
			return
		}
	}
}

type client struct {
	e    *env
	http *http.Client
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

func (c *client) do(method, path string, body any, out any, headers ...string) int {
	c.e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.e.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

func (c *client) expect(want int, method, path string, body any, out any, headers ...string) {
	c.e.t.Helper()
	var raw json.RawMessage
	if got := c.do(method, path, body, &raw, headers...); got != want {
		c.e.t.Fatalf("%s %s = %d, want %d: %s", method, path, got, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			c.e.t.Fatal(err)
		}
	}
}

func (c *client) signup(email string) {
	c.e.t.Helper()
	c.expect(http.StatusCreated, "POST", "/api/auth/signup", map[string]string{"email": email, "password": "a long enough password"}, nil)
}

func notes(t *testing.T, n int) string {
	b, err := os.ReadFile(filepath.Join("../../testdata/eval-campaign/sessions", fmt.Sprintf("%02d.md", n)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type session struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
	Status string `json:"status"`
}

func TestFullSessionLifecycle(t *testing.T) {
	e := newEnv(t)
	gm, player, outsider := e.client(), e.client(), e.client()
	gm.signup("gm@example.com")
	player.signup("player@example.com")
	outsider.signup("outsider@example.com")

	var camp struct{ ID string }
	gm.expect(http.StatusCreated, "POST", "/api/campaigns", map[string]any{
		"name":  "The Lantern Coast",
		"party": []map[string]any{{"name": "Iska Vale", "aliases": []string{"Iska"}}, {"name": "Dorran Holt", "aliases": []string{"Dorran"}}, {"name": "Sefa Quill", "aliases": []string{"Sefa"}}},
	}, &camp)
	base := "/api/campaigns/" + camp.ID
	gm.expect(http.StatusCreated, "POST", base+"/members", map[string]string{"email": "player@example.com", "role": "player"}, nil)

	// --- permissions before any canon exists ---
	outsider.expect(http.StatusNotFound, "GET", base, nil, nil)
	player.expect(http.StatusForbidden, "POST", base+"/sessions", map[string]string{"notes": "I write canon now"}, nil)
	player.expect(http.StatusForbidden, "POST", base+"/members", map[string]string{"email": "outsider@example.com", "role": "gm"}, nil)

	// --- session 1: submit, watch the status stream, process ---
	var s1 session
	gm.expect(http.StatusAccepted, "POST", base+"/sessions", map[string]string{"notes": notes(t, 1), "played_on": "2026-11-01"}, &s1)
	if s1.Number != 1 || s1.Status != "queued" {
		t.Fatalf("new session = %+v", s1)
	}
	player.expect(http.StatusNotFound, "GET", "/api/sessions/"+s1.ID, nil, nil) // raw notes are GM-only

	statuses := gm.stream("/api/sessions/" + s1.ID + "/events")
	time.Sleep(100 * time.Millisecond) // let the stream subscribe before the worker runs
	e.drain()
	if got := collect(t, statuses); strings.Join(got, ",") != "queued,processing,review" {
		t.Errorf("status stream = %v, want queued,processing,review", got)
	}

	var detail struct {
		Session session `json:"session"`
		Recaps  []struct {
			Audience  string `json:"audience"`
			Body      string `json:"body"`
			Published bool   `json:"published"`
		} `json:"recaps"`
	}
	gm.expect(http.StatusOK, "GET", "/api/sessions/"+s1.ID, nil, &detail)
	if detail.Session.Status != "review" || len(detail.Recaps) != 2 {
		t.Fatalf("after processing: %+v", detail)
	}
	var prop struct {
		Stale    bool           `json:"stale"`
		Proposal canon.Proposal `json:"proposal"`
	}
	gm.expect(http.StatusOK, "GET", "/api/sessions/"+s1.ID+"/proposal", nil, &prop)
	if len(prop.Proposal.Facts) == 0 || prop.Stale {
		t.Fatalf("proposal should have facts and be fresh: %+v", prop)
	}
	player.expect(http.StatusNotFound, "GET", "/api/sessions/"+s1.ID+"/proposal", nil, nil)

	// Nothing is visible to players until the GM approves.
	if c := player.canon(base); len(c.Facts) != 0 || len(player.recaps(base)) != 0 {
		t.Fatalf("player sees unapproved canon: %d facts", len(c.Facts))
	}
	gm.expect(http.StatusOK, "POST", "/api/sessions/"+s1.ID+"/approve", nil, nil)

	// --- after approval: same endpoint, different views ---
	gmView, playerView := gm.canonRaw(base), player.canonRaw(base)
	if !strings.Contains(gmView, "Pale Tide") {
		t.Error("GM canon should include the secret cult")
	}
	for _, secret := range []string{"Pale Tide", "first to vanish"} {
		if strings.Contains(playerView, secret) {
			t.Errorf("player canon leaks %q", secret)
		}
	}
	recaps := player.recaps(base)
	if len(recaps) != 1 || strings.Contains(recaps[0], "Pale Tide") {
		t.Errorf("player recaps = %v", recaps)
	}
	if !strings.Contains(playerView, "Oswin") {
		t.Error("player canon should include party-visible NPCs")
	}

	// --- sessions 2 and 3: approval order and stale proposals ---
	var s2, s3 session
	gm.expect(http.StatusAccepted, "POST", base+"/sessions", map[string]string{"notes": notes(t, 2)}, &s2)
	gm.expect(http.StatusAccepted, "POST", base+"/sessions", map[string]string{"notes": notes(t, 3)}, &s3)
	e.drain()
	gm.expect(http.StatusConflict, "POST", "/api/sessions/"+s3.ID+"/approve", nil, nil) // session 2 first
	gm.expect(http.StatusOK, "POST", "/api/sessions/"+s2.ID+"/approve", nil, nil)

	// Session 3 was processed before session 2's canon existed, so approving s2 re-queued it.
	var s3now struct{ Session session }
	gm.expect(http.StatusOK, "GET", "/api/sessions/"+s3.ID, nil, &s3now)
	if s3now.Session.Status != "queued" {
		t.Fatalf("stale session 3 should be re-queued, got %q", s3now.Session.Status)
	}
	e.drain()
	gm.expect(http.StatusOK, "POST", "/api/sessions/"+s3.ID+"/approve", nil, nil)
	gm.expect(http.StatusConflict, "POST", "/api/sessions/"+s3.ID+"/approve", nil, nil) // already approved

	// Session 3 is where the planted "daughter's name" contradiction lives.
	gm.expect(http.StatusOK, "GET", "/api/sessions/"+s3.ID+"/proposal", nil, &prop)
	if len(prop.Proposal.Contradictions) == 0 {
		t.Error("session 3 should report the Elsabet/Mirelle contradiction")
	}

	var list struct{ Sessions []session }
	gm.expect(http.StatusOK, "GET", base+"/sessions", nil, &list)
	if len(list.Sessions) != 3 {
		t.Fatalf("sessions = %+v", list.Sessions)
	}
	player.expect(http.StatusForbidden, "GET", base+"/sessions", nil, nil)

	// --- account flows ---
	gm.expect(http.StatusNoContent, "POST", "/api/auth/logout", nil, nil)
	gm.expect(http.StatusUnauthorized, "GET", "/api/me", nil, nil)
}

func TestMagicLinkSignIn(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	c.expect(http.StatusAccepted, "POST", "/api/auth/magic-link", map[string]string{"email": "new@example.com"}, nil)

	var mailbox struct{ Messages []mail.Message }
	c.expect(http.StatusOK, "GET", "/dev/mailbox", nil, &mailbox)
	if len(mailbox.Messages) != 1 {
		t.Fatalf("mailbox = %+v", mailbox)
	}
	link := mailbox.Messages[0].Body[strings.Index(mailbox.Messages[0].Body, "http"):]
	link = strings.Fields(link)[0]
	u, _ := url.Parse(link)

	var me struct{ User auth.User }
	c.expect(http.StatusOK, "POST", "/api/auth/magic-link/verify", map[string]string{"token": u.Query().Get("token")}, &me)
	c.expect(http.StatusOK, "GET", "/api/me", nil, &me)
	if me.User.Email != "new@example.com" {
		t.Fatalf("me = %+v", me)
	}
	c.expect(http.StatusUnauthorized, "POST", "/api/auth/magic-link/verify", map[string]string{"token": u.Query().Get("token")}, nil)
}

func TestRequestHardening(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	c.signup("gm@example.com")

	// Cross-site browser requests to state-changing endpoints are rejected.
	c.expect(http.StatusForbidden, "POST", "/api/campaigns", map[string]string{"name": "x"}, nil, "Sec-Fetch-Site", "cross-site")
	// Bodies must be JSON with the right content type and no unknown fields.
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/campaigns", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("form body should be rejected, got %d", resp.StatusCode)
	}
	c.expect(http.StatusBadRequest, "POST", "/api/campaigns", map[string]string{"name": "x", "owner_id": "someone-else"}, nil)
	// Unauthenticated access.
	e.client().expect(http.StatusUnauthorized, "GET", "/api/campaigns", nil, nil)
	// Duplicate signups and bad passwords.
	e.client().expect(http.StatusConflict, "POST", "/api/auth/signup", map[string]string{"email": "GM@example.com", "password": "a long enough password"}, nil)
	e.client().expect(http.StatusBadRequest, "POST", "/api/auth/signup", map[string]string{"email": "x@example.com", "password": "short"}, nil)

	// Login attempts are rate-limited per IP.
	limited := false
	for range 30 {
		if e.client().do("POST", "/api/auth/login", map[string]string{"email": "gm@example.com", "password": "wrong password!!"}, nil) == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("repeated failed logins should be rate-limited")
	}
}

// ---- helpers ----

type canonView struct {
	Role  string      `json:"role"`
	Canon canon.Canon `json:"canon"`
}

func (c *client) canonRaw(base string) string {
	c.e.t.Helper()
	var raw json.RawMessage
	c.expect(http.StatusOK, "GET", base+"/canon", nil, &raw)
	return string(raw)
}

func (c *client) canon(base string) canon.Canon {
	var v canonView
	_ = json.Unmarshal([]byte(c.canonRaw(base)), &v)
	return v.Canon
}

func (c *client) recaps(base string) []string {
	c.e.t.Helper()
	var out struct{ Recaps []struct{ Body string } }
	c.expect(http.StatusOK, "GET", base+"/recaps", nil, &out)
	var bodies []string
	for _, r := range out.Recaps {
		bodies = append(bodies, r.Body)
	}
	return bodies
}

// stream opens an SSE stream and sends each status it reports on the channel,
// closing the channel when the server ends the stream.
func (c *client) stream(path string) <-chan string {
	c.e.t.Helper()
	req, _ := http.NewRequest("GET", c.e.srv.URL+path, nil)
	resp, err := c.http.Do(req) //nolint:bodyclose // closed by the reader goroutine below
	if err != nil {
		c.e.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		c.e.t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := make(chan string, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var ev notify.SessionEvent
				if json.Unmarshal([]byte(data), &ev) == nil {
					out <- ev.Status
				}
			}
		}
	}()
	return out
}

func collect(t *testing.T, ch <-chan string) []string {
	t.Helper()
	var got []string
	timeout := time.After(10 * time.Second)
	for {
		select {
		case s, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, s)
		case <-timeout:
			t.Fatalf("stream did not finish; got %v", got)
		}
	}
}
