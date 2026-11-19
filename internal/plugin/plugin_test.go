package plugin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/sketchain/cpa-quota/internal/ccbilling"
	"github.com/sketchain/cpa-quota/internal/pgtest"
)

const ratioTable = `{"success":true,"data":{"model_ratio":{"claude-opus-4-8":2.5,"gpt-5.6-sol":2},
	"completion_ratio":{"claude-opus-4-8":5,"gpt-5.6-sol":8},"cache_ratio":{"claude-opus-4-8":0.1},"create_cache_ratio":{"claude-opus-4-8":1.25}}}`

// priceServer serves the ratio table until it is told to fail.
type priceServer struct {
	*httptest.Server
	failing  atomic.Bool
	requests atomic.Int64
}

func newPriceServer(t *testing.T) *priceServer {
	p := &priceServer{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p.requests.Add(1)
		if p.failing.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(ratioTable))
	}))
	t.Cleanup(p.Close)
	return p
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	t      *testing.T
	app    *App
	clock  *testClock
	prices *priceServer
	db     *sql.DB
	schema string
	logs   *[]string
}

// newHarness registers a plugin on a fresh schema; dsn overrides the
// database it connects to, so a proxy can stand between them.
func newHarness(t *testing.T, dsn string, extra string) *harness {
	t.Helper()
	schema := pgtest.Schema(t)
	if dsn == "" {
		dsn = pgtest.DSN(t)
	}
	h := &harness{t: t, app: NewApp(), clock: &testClock{now: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}, prices: newPriceServer(t), schema: schema}
	logs := []string{}
	var mu sync.Mutex
	h.logs = &logs
	h.app.Clock = h.clock.Now
	h.app.SetHostLogger(func(level, message string) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, level+": "+message)
	})
	base := fmt.Sprintf("enabled: true\ninstance: cc-1\npostgres_dsn: %q\npostgres_schema: %s\napi_token: read-token\nadmin_token: admin-token\n"+
		"price_url: %s\nsubscription: claude\ntier: max5x\ndb_timeout_seconds: 2\n", dsn, schema, h.prices.URL)
	// Keys in extra replace the base ones.
	var lines []string
	for _, line := range strings.Split(base, "\n") {
		key, _, _ := strings.Cut(line, ":")
		if key == "" || !strings.Contains("\n"+extra, "\n"+key+":") {
			lines = append(lines, line)
		}
	}
	config := strings.Join(lines, "\n") + "\n" + extra
	h.call(MethodPluginRegister, LifecycleRequest{ConfigYAML: []byte(config)})
	h.call(MethodManagementRegister, ManagementRegistrationRequest{ResourceBasePath: "/v0/resource/plugins/cpa-quota"})
	t.Cleanup(h.app.Shutdown)
	parsed, err := pgx.ParseConfig(pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	parsed.RuntimeParams["search_path"] = `"` + schema + `"`
	h.db = stdlib.OpenDB(*parsed)
	t.Cleanup(func() { h.db.Close() })
	return h
}

func (h *harness) call(method string, request any) json.RawMessage {
	h.t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		h.t.Fatal(err)
	}
	var reply envelope
	if err := json.Unmarshal(h.app.HandleMethod(method, payload), &reply); err != nil {
		h.t.Fatal(err)
	}
	if !reply.OK {
		h.t.Fatalf("%s failed: %+v", method, reply.Error)
	}
	return reply.Result
}

func (h *harness) get(route, token string) (int, map[string]any) {
	h.t.Helper()
	request := ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/cpa-quota/" + route, Headers: http.Header{}}
	if base, query, found := strings.Cut(route, "?"); found {
		request.Path = "/v0/resource/plugins/cpa-quota/" + base
		request.Query = map[string][]string{}
		for _, pair := range strings.Split(query, "&") {
			key, value, _ := strings.Cut(pair, "=")
			request.Query[key] = append(request.Query[key], value)
		}
	}
	if token != "" {
		request.Headers.Set("Authorization", "Bearer "+token)
	}
	var response ManagementResponse
	if err := json.Unmarshal(h.call(MethodManagementHandle, request), &response); err != nil {
		h.t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body, &body); err != nil {
		h.t.Fatalf("body %s: %v", response.Body, err)
	}
	return response.StatusCode, body
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// claudeRecord is a usage record of a Claude OAuth request whose response
// reported the unified windows.
func claudeRecord(id string, at time.Time, used5, used7 float64) map[string]any {
	return map[string]any{
		"RequestID": id, "Provider": "claude", "ExecutorType": "ClaudeExecutor", "Model": "claude-opus-4-8", "AuthIndex": "acct-1", "AuthType": "oauth",
		"RequestedAt": at, "Latency": int64(2 * time.Second), "Stream": false,
		"Detail": map[string]any{"InputTokens": 1000, "OutputTokens": 2000, "ReasoningTokens": 500, "CacheReadTokens": 10000, "CacheCreationTokens": 4000},
		"ResponseHeaders": map[string][]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": {fmt.Sprint(used5 / 100)},
			"Anthropic-Ratelimit-Unified-5h-Reset":       {fmt.Sprint(at.Add(3 * time.Hour).Unix())},
			"Anthropic-Ratelimit-Unified-7d-Utilization": {fmt.Sprint(used7 / 100)},
			"Anthropic-Ratelimit-Unified-7d-Reset":       {fmt.Sprint(at.Add(4 * 24 * time.Hour).Unix())},
		},
	}
}

func TestConfigRequiresInstanceAndDSN(t *testing.T) {
	for _, test := range []struct{ yaml, want string }{
		{"postgres_dsn: x", "instance is required"},
		{"instance: a", "postgres_dsn is required"},
		{"instance: a\npostgres_dsn: x\nsubscription: gemini", "subscription must be claude or chatgpt"},
		{"instance: a\npostgres_dsn: x\nsubscription: chatgpt\ntier: max5x", `tier "max5x" is not a tier of chatgpt`},
		{"instance: a\npostgres_dsn: x\ntier_k: {team: 3}", `"team" is not a tier of claude`},
		{"instance: a\npostgres_dsn: x\nsafety_factor: 1.5", "safety_factor"},
		{"instance: a\npostgres_dsn: x\nestimation: {window_percent: 0}", "window_percent"},
		{"instance: a\npostgres_dsn: x\ntrend_timezone: Mars/Base", "trend_timezone"},
		{"instance: a\npostgres_dsn: x\nprice_url: ftp://x", "price_url"},
	} {
		if _, err := ParseConfig([]byte(test.yaml)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%q: %v", test.yaml, err)
		}
	}
	config, err := ParseConfig([]byte("instance: cc-1\npostgres_dsn: postgres://x\nsubscription: ChatGPT\nmanual_k: 7\n" +
		"model_weights: {enabled: true, items: [{id: spark, models: [gpt-5.3-codex-spark]}]}"))
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := config.Settings()
	params := settings.ParamsFor(ccbilling.ProviderCodex)
	if config.Provider() != ccbilling.ProviderCodex || *config.ManualK != 7 || config.PriceURL != DefaultPriceURL || config.SafetyFactor != 0.9 ||
		params.ColdStartBlocks != 20 || params.WindowPercent != 8 || params.BlockGapMinutes != 5 || params.RecentDays != 14 ||
		!settings.Providers[ccbilling.ProviderCodex].Weights.Enabled || config.AccountAuthTypes[0] != "oauth" {
		t.Fatalf("config = %+v", config)
	}
}

func TestUsageIsPricedRecordedAndObserved(t *testing.T) {
	h := newHarness(t, "", "")
	at := h.clock.Now().Add(-time.Minute)
	h.call(MethodUsageHandle, claudeRecord("req-1", at, 12, 34))
	// An API key credential and another provider do not count.
	apiKey := claudeRecord("req-2", at, 99, 99)
	apiKey["AuthType"] = "apikey"
	h.call(MethodUsageHandle, apiKey)
	codex := claudeRecord("req-3", at, 99, 99)
	codex["Provider"] = "codex"
	h.call(MethodUsageHandle, codex)
	var cost float64
	var model, accounting string
	var output int64
	var mainWindow, priced bool
	if err := h.db.QueryRow(`SELECT cost_usd, model, accounting, output_tokens, main_window, priced FROM requests WHERE instance = 'cc-1' AND request_id = 'req-1'`).
		Scan(&cost, &model, &accounting, &output, &mainWindow, &priced); err != nil {
		t.Fatal(err)
	}
	// $5 input, $25 output, $0.50 cache read, $6.25 cache write per 1M;
	// reasoning is inside Claude's output.
	want := (1000*5 + 2000*25 + 10000*0.5 + 4000*6.25) / 1e6
	if math.Abs(cost-want) > 1e-12 || model != "claude-opus-4-8" || accounting != "complete" || output != 2000 || !mainWindow || !priced {
		t.Fatalf("request = %v %s %s %d %v %v", cost, model, accounting, output, mainWindow, priced)
	}
	if n := h.count(`SELECT count(*) FROM requests`); n != 1 {
		t.Fatalf("requests = %d", n)
	}
	if n := h.count(`SELECT count(*) FROM quota_report_windows WHERE instance = 'cc-1' AND limit_key = 'unified'`); n != 2 {
		t.Fatalf("windows = %d", n)
	}
	// Redelivery changes nothing.
	h.call(MethodUsageHandle, claudeRecord("req-1", at, 12, 34))
	if n := h.count(`SELECT count(*) FROM requests`); n != 1 {
		t.Fatalf("requests after redelivery = %d", n)
	}
	status, body := h.get("quota", "read-token")
	if status != http.StatusOK {
		t.Fatalf("quota = %d %v", status, body)
	}
	accounts := body["accounts"].([]any)
	account := accounts[0].(map[string]any)
	week := account["week"].(map[string]any)
	if len(accounts) != 1 || account["auth_index"] != "acct-1" || account["state"] != "cold" || account["tier"] != "max5x" ||
		account["k_source"] != "default" || week["used_percent"] != 34.0 || week["total_usd"] != 1250.0 ||
		math.Abs(week["remaining_usd"].(float64)-0.9*1250*0.66) > 1e-9 {
		t.Fatalf("account = %v", account)
	}
}

func TestQuotaAPIAuthenticationAndFields(t *testing.T) {
	h := newHarness(t, "", "")
	for _, token := range []string{"", "wrong", "admin-token"} {
		if status, body := h.get("quota", token); status != http.StatusUnauthorized || body["error"] == nil {
			t.Fatalf("token %q: %d %v", token, status, body)
		}
	}
	if status, _ := h.get("admin/refresh-prices", "read-token"); status != http.StatusUnauthorized {
		t.Fatalf("admin with the read token: %d", status)
	}
	if status, _ := h.get("nowhere", "read-token"); status != http.StatusNotFound {
		t.Fatalf("unknown route: %d", status)
	}
	status, body := h.get("quota", "read-token")
	if status != http.StatusOK {
		t.Fatalf("quota = %d", status)
	}
	for _, field := range []string{"plugin", "version", "instance", "generated_at", "subscription", "tier", "price_table", "database", "estimates", "accounts"} {
		if _, ok := body[field]; !ok {
			t.Fatalf("missing %s in %v", field, body)
		}
	}
	prices := body["price_table"].(map[string]any)
	database := body["database"].(map[string]any)
	if body["plugin"] != PluginID || body["version"] != Version || body["instance"] != "cc-1" || prices["origin"] != "remote" ||
		prices["models"] != 2.0 || database["status"] != "ok" || database["schema_version"] != 1.0 {
		t.Fatalf("quota = %v", body)
	}
	// Disabled admin operations say so.
	disabled := newHarness(t, "", "admin_token: ''\n")
	if status, body := disabled.get("admin/rebuild", "admin-token"); status != http.StatusUnauthorized ||
		!strings.Contains(fmt.Sprint(body), "admin_token is not configured") {
		t.Fatalf("disabled admin: %d %v", status, body)
	}
}

func TestEstimatedAccountsInTheQuotaAPI(t *testing.T) {
	h := newHarness(t, "", "estimation: {cold_start_blocks: 3}\nauth_indexes: [acct-1]\n")
	// Steady usage: each request costs about $0.12 and raises the 5-hour
	// window by a point (5-hour total about $12) and the 7-day window by a
	// sixth of one (k = 6).
	start := h.clock.Now().Add(-30 * time.Hour)
	used5, used7 := 0.0, 0.0
	for block := 0; block < 6; block++ {
		blockStart := start.Add(time.Duration(block) * 4 * time.Hour)
		used5 = 0
		for i := 0; i < 40; i++ {
			at := blockStart.Add(time.Duration(i) * time.Minute)
			record := claudeRecord(fmt.Sprintf("b%d-%d", block, i), at, used5, used7)
			record["ResponseHeaders"].(map[string][]string)["Anthropic-Ratelimit-Unified-5h-Reset"] = []string{fmt.Sprint(blockStart.Add(5 * time.Hour).Unix())}
			record["ResponseHeaders"].(map[string][]string)["Anthropic-Ratelimit-Unified-7d-Reset"] = []string{fmt.Sprint(start.Add(6 * 24 * time.Hour).Unix())}
			h.call(MethodUsageHandle, record)
			used5++
			used7 += 1.0 / 6
		}
	}
	// Estimates refresh at most once a minute.
	h.clock.Add(time.Minute)
	status, body := h.get("quota", "read-token")
	if status != http.StatusOK {
		t.Fatalf("quota = %d", status)
	}
	account := body["accounts"].([]any)[0].(map[string]any)
	five, estimate := account["five_hour"].(map[string]any), account["estimate"].(map[string]any)
	cost := (1000*5 + 2000*25 + 10000*0.5 + 4000*6.25) / 1e6
	if account["state"] != "estimated" || math.Abs(five["total_usd"].(float64)-100*cost)/(100*cost) > 0.05 || estimate["clean_blocks"].(float64) < 3 ||
		account["k_source"] != "auto" || math.Abs(account["k"].(float64)-6) > 0.5 || body["estimates"].(map[string]any)["loaded"] != true {
		t.Fatalf("account = %v", account)
	}
	forecast := five["forecast"].(map[string]any)
	if forecast["source"] == "" || forecast["lookback_seconds"] != 3600.0 {
		t.Fatalf("forecast = %v", forecast)
	}
	if n := h.count(`SELECT count(*) FROM estimates WHERE instance = 'cc-1' AND state = 'estimated'`); n != 1 {
		t.Fatalf("estimate rows = %d", n)
	}
	if n := h.count(`SELECT count(*) FROM k_trend_daily WHERE instance = 'cc-1'`); n == 0 {
		t.Fatal("no daily trend")
	}
	// A rebuild recreates the same estimate.
	status, rebuilt := h.get("admin/rebuild", "admin-token")
	again := rebuilt["accounts"].([]any)[0].(map[string]any)
	if status != http.StatusOK || again["estimate"].(map[string]any)["segments"] != estimate["segments"] {
		t.Fatalf("rebuild = %d %v", status, rebuilt)
	}
}

func TestSafeguardEventsFromStreamingAndNonStreamingRefusals(t *testing.T) {
	h := newHarness(t, "", "")
	at := h.clock.Now().Add(-time.Minute)
	refusal := func(id string, stream bool) map[string]any {
		record := claudeRecord(id, at, 1, 1)
		// CLIProxyAPI publishes a refusal as a 400 failure whose body is
		// clienterror.ClaudePolicyRefusalMessage, streaming or not.
		record["Failed"], record["Stream"] = true, stream
		record["Failure"] = map[string]any{"StatusCode": 400, "Body": "claude refusal (cyber): The request could facilitate attacks."}
		return record
	}
	h.call(MethodUsageHandle, refusal("nonstream-1", false))
	h.call(MethodUsageHandle, refusal("stream-1", true))
	other := claudeRecord("overloaded-1", at, 1, 1)
	other["Failed"] = true
	other["Failure"] = map[string]any{"StatusCode": 529, "Body": `{"type":"error","error":{"type":"overloaded_error"}}`}
	h.call(MethodUsageHandle, other)
	rows, err := h.db.Query(`SELECT request_id, category, reason, status_code, stream, auth_index, model, instance FROM safeguard_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, category, reason, authIndex, model, instance string
		var status int
		var stream bool
		if err := rows.Scan(&id, &category, &reason, &status, &stream, &authIndex, &model, &instance); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s|%s|%s|%d|%v|%s|%s|%s", id, category, reason, status, stream, authIndex, model, instance))
	}
	want := []string{
		"nonstream-1|cyber|The request could facilitate attacks.|400|false|acct-1|claude-opus-4-8|cc-1",
		"stream-1|cyber|The request could facilitate attacks.|400|true|acct-1|claude-opus-4-8|cc-1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("safeguard events:\n%s", strings.Join(got, "\n"))
	}
	// The refused requests are still recorded as failed requests.
	if n := h.count(`SELECT count(*) FROM requests WHERE failed`); n != 3 {
		t.Fatalf("failed requests = %d", n)
	}
}

func TestPricesRefreshDailyAndFallBackToTheStoredTable(t *testing.T) {
	h := newHarness(t, "", "")
	if h.prices.requests.Load() != 1 {
		t.Fatalf("price fetches at registration = %d", h.prices.requests.Load())
	}
	at := h.clock.Now()
	h.call(MethodUsageHandle, claudeRecord("r1", at, 1, 1))
	if h.prices.requests.Load() != 1 {
		t.Fatal("refreshed a fresh table")
	}
	// After a day usage handling refreshes it; a failure keeps the table and
	// is retried only after the retry period.
	h.prices.failing.Store(true)
	h.clock.Add(25 * time.Hour)
	h.call(MethodUsageHandle, claudeRecord("r2", h.clock.Now(), 1, 1))
	h.call(MethodUsageHandle, claudeRecord("r3", h.clock.Now(), 1, 1))
	if h.prices.requests.Load() != 2 {
		t.Fatalf("price fetches = %d", h.prices.requests.Load())
	}
	var priced int
	if priced = h.count(`SELECT count(*) FROM requests WHERE priced AND cost_usd > 0`); priced != 3 {
		t.Fatalf("priced requests = %d", priced)
	}
	_, body := h.get("quota", "read-token")
	if table := body["price_table"].(map[string]any); table["origin"] != "remote" || table["last_error"] == nil {
		t.Fatalf("price table = %v", table)
	}
	h.clock.Add(6 * time.Minute)
	h.call(MethodUsageHandle, claudeRecord("r4", h.clock.Now(), 1, 1))
	if h.prices.requests.Load() != 3 {
		t.Fatalf("price fetches after the retry period = %d", h.prices.requests.Load())
	}
	// The manual refresh reports the failure, then succeeds.
	if status, _ := h.get("admin/refresh-prices", "admin-token"); status != http.StatusBadGateway {
		t.Fatalf("manual refresh while down = %d", status)
	}
	h.prices.failing.Store(false)
	if status, body := h.get("admin/refresh-prices", "admin-token"); status != http.StatusOK || body["ok"] != true {
		t.Fatalf("manual refresh = %d %v", status, body)
	}

	// Another start of the instance while the price server is down uses
	// the stored table.
	h.prices.failing.Store(true)
	restarted := NewApp()
	restarted.Clock = h.clock.Now
	config := fmt.Sprintf("instance: cc-1\npostgres_dsn: %q\npostgres_schema: %s\nprice_url: %s\napi_token: t\n", pgtest.DSN(t), h.schema, h.prices.URL)
	payload, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte(config)})
	restarted.HandleMethod(MethodPluginRegister, payload)
	defer restarted.Shutdown()
	if status := restarted.Quota().PriceTable; status.Origin != PriceOriginDatabase || status.Models != 2 {
		t.Fatalf("restarted price table = %+v", status)
	}
	// Without the table a request is unpriced; the stored table prices it.
	restarted.HandleUsage(decode(t, claudeRecord("r5", h.clock.Now(), 1, 1)))
	if n := h.count(`SELECT count(*) FROM requests WHERE request_id = 'r5' AND priced`); n != 1 {
		t.Fatal("the stored table did not price the request")
	}
}

func decode(t *testing.T, record map[string]any) UsageRecord {
	t.Helper()
	raw, _ := json.Marshal(record)
	var usage UsageRecord
	if err := json.Unmarshal(raw, &usage); err != nil {
		t.Fatal(err)
	}
	return usage
}

func TestDatabaseUnavailableAtStartThenReconnectsLoadsAndFlushes(t *testing.T) {
	proxy, dsn := pgtest.NewProxy(t)
	proxy.Down()
	h := newHarness(t, dsn, "auth_indexes: [acct-1]\n")
	// Registration succeeded without the database.
	status, body := h.get("quota", "read-token")
	database := body["database"].(map[string]any)
	if status != http.StatusOK || database["status"] != "unavailable" || database["last_error"] == nil {
		t.Fatalf("quota without a database = %d %v", status, body)
	}
	account := body["accounts"].([]any)[0].(map[string]any)
	if account["state"] != "cold" || body["estimates"].(map[string]any)["loaded"] != false {
		t.Fatalf("account without a database = %v", account)
	}
	// Records wait in memory, in order.
	at := h.clock.Now().Add(-time.Minute)
	for i := 1; i <= 3; i++ {
		h.call(MethodUsageHandle, claudeRecord(fmt.Sprintf("q%d", i), at.Add(time.Duration(i)*time.Second), float64(i), 1))
	}
	// The price table fetched at registration waits with them.
	if pending := h.app.Quota().Database.PendingWrites; pending != 4 {
		t.Fatalf("pending = %d", pending)
	}
	if live := h.app.Quota().Accounts[0]; live.FiveHour.UsedPercent != 3 {
		t.Fatalf("live utilization = %+v", live.FiveHour)
	}
	// The database returns; within a minute nothing retries it.
	proxy.Up()
	h.call(MethodUsageHandle, claudeRecord("q4", at.Add(4*time.Second), 4, 1))
	if pending := h.app.Quota().Database.PendingWrites; pending != 5 {
		t.Fatalf("pending before the retry interval = %d", pending)
	}
	h.clock.Add(time.Minute)
	h.call(MethodUsageHandle, claudeRecord("q5", at.Add(5*time.Second), 5, 1))
	quota := h.app.Quota()
	if quota.Database.Status != "ok" || quota.Database.PendingWrites != 0 || !quota.Estimates.Loaded {
		t.Fatalf("after reconnecting = %+v", quota.Database)
	}
	rows, err := h.db.Query(`SELECT request_id FROM requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	if strings.Join(ids, ",") != "q1,q2,q3,q4,q5" {
		t.Fatalf("stored in order %v", ids)
	}
	if n := h.count(`SELECT count(*) FROM estimates WHERE instance = 'cc-1'`); n != 1 {
		t.Fatalf("estimate rows = %d", n)
	}
	// The price table fetched before the database came back is stored too.
	if n := h.count(`SELECT count(*) FROM price_tables WHERE instance = 'cc-1'`); n != 1 {
		t.Fatalf("price tables = %d", n)
	}
}
