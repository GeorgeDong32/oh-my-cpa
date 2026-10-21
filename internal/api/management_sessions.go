package api

// Session statistics read APIs: client-side coding session usage in DTOs
// resolved against the viewer's calendar (docs/plans/session-stats.md §6).
// Reads are not audited (console precedent); rescan is audited and refused
// in demo mode like every write that needs a live filesystem.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oh-my-cpa/oh-my-cpa/internal/pricing"
	"github.com/oh-my-cpa/oh-my-cpa/internal/repository"
	"github.com/oh-my-cpa/oh-my-cpa/internal/sessionstats"
	"github.com/oh-my-cpa/oh-my-cpa/internal/timezone"
)

// SessionStatsReader is the store surface the handlers read; the repository
// satisfies it directly.
type SessionStatsReader interface {
	SummaryRows(ctx context.Context, fromBucket int64) ([]sessionstats.AggregatedRow, error)
	ClientStatusRows(ctx context.Context) ([]sessionstats.ClientStatusRow, error)
	// BucketSessionCounts counts distinct source sessions per 15-minute
	// bucket from the activity table — the exact day/window session numbers.
	BucketSessionCounts(ctx context.Context, fromBucket int64) (map[int64]int64, error)
}

// SessionStatsScanner triggers the background scan loop.
type SessionStatsScanner interface {
	TriggerScan() bool
}

const sessionsMaxDays = 365

// sessionsTimezone resolves the viewer's calendar like the heatmap does: an
// explicit IANA name, refused rather than defaulted when unknown.
func sessionsTimezone(request *http.Request) (*time.Location, string) {
	raw := strings.TrimSpace(request.URL.Query().Get("tz"))
	if raw == "" {
		return time.UTC, ""
	}
	if len(raw) > 64 {
		return nil, "tz is not a valid timezone name"
	}
	zone, err := timezone.Load(raw)
	if err != nil {
		return nil, fmt.Sprintf("tz %q is not a known timezone", raw)
	}
	return zone, ""
}

func sessionsDays(request *http.Request) (int, string) {
	raw := strings.TrimSpace(request.URL.Query().Get("days"))
	if raw == "" {
		return 30, ""
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 || days > sessionsMaxDays {
		return 0, "days must be an integer between 1 and 365"
	}
	return days, ""
}

type sessionsTotals struct {
	Input, Output, CacheRead, CacheWrite, Reasoning int64
}

func (t *sessionsTotals) add(row sessionstats.AggregatedRow) {
	t.Input += row.Input
	t.Output += row.Output
	t.CacheRead += row.CacheRead
	t.CacheWrite += row.CacheWrite
	t.Reasoning += row.Reasoning
}

func (t sessionsTotals) total() int64 {
	return t.Input + t.Output + t.CacheRead + t.CacheWrite + t.Reasoning
}

// dayKey is the local calendar date of a bucket under the viewer's zone.
func dayKey(bucketMS int64, zone *time.Location) string {
	return time.UnixMilli(bucketMS).In(zone).Format("2006-01-02")
}

// foldDaily buckets the UTC grid rows into local days.
func foldDaily(rows []sessionstats.AggregatedRow, zone *time.Location) map[string]*sessionDayDTO {
	days := map[string]*sessionDayDTO{}
	for _, row := range rows {
		key := dayKey(row.Bucket15MS, zone)
		day := days[key]
		if day == nil {
			day = &sessionDayDTO{Date: key}
			days[key] = day
		}
		day.Input += row.Input
		day.Output += row.Output
		day.CacheRead += row.CacheRead
		day.CacheWrite += row.CacheWrite
		day.Reasoning += row.Reasoning
		day.Messages += row.Messages
	}
	return days
}

type sessionDayDTO struct {
	Date       string  `json:"date"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Reasoning  int64   `json:"reasoning"`
	Messages   int64   `json:"messages"`
	Sessions   int64   `json:"sessions"`
	CostUSD    float64 `json:"cost_usd"`
}

type sessionTokenMixDTO struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
}

type sessionModelRowDTO struct {
	Model   string  `json:"model"`
	Clients string  `json:"clients"`
	Tokens  int64   `json:"tokens"`
	Share   float64 `json:"share"`
	CostUSD float64 `json:"cost_usd"`
	Priced  bool    `json:"priced"`
}

type sessionClientRowDTO struct {
	Client     string `json:"client"`
	Tokens     int64  `json:"tokens"`
	Sessions   int64  `json:"sessions"`
	Entries    int64  `json:"entries"`
	Files      int64  `json:"files"`
	Unreadable int64  `json:"unreadable"`
	Oversized  int64  `json:"oversized"`
	LastScanMS int64  `json:"last_scan_ms"`
}

type sessionsSummaryDTO struct {
	Window         sessionTokenMixDTO   `json:"tokens"`
	TotalTokens    int64                `json:"total_tokens"`
	CostUSD        float64              `json:"cost_usd"`
	CostSource     string               `json:"cost_source"`
	UnpricedTokens int64                `json:"unpriced_tokens"`
	ActiveDays     int64                `json:"active_days"`
	Sessions       int64                `json:"sessions"`
	Today          *sessionDayDTO       `json:"today,omitempty"`
	BiggestDay     string               `json:"biggest_day,omitempty"`
	Weekday        map[string]int64     `json:"weekday,omitempty"`
	Models         []sessionModelRowDTO `json:"models"`
}

type sessionsDailyDTO struct {
	Days []sessionDayDTO `json:"days"`
}

type sessionsClientsDTO struct {
	Clients []sessionClientRowDTO `json:"clients"`
}

func (h *Handler) sessionsReader(writer http.ResponseWriter, request *http.Request) (SessionStatsReader, bool) {
	if h.repo == nil {
		writeError(writer, http.StatusServiceUnavailable, "database is unavailable")
		return nil, false
	}
	return h.repo, true
}

// estimateSessionCost prices the rows against the current book through the
// same Quote the gateway uses: base rates, the model multiplier, uncached
// prompt / cache-read / cache-write / completion buckets (spec §5 — no
// tiers, no channel multiplier). Unpriced models add to the unpriced share
// instead of $0. The console's exclusive buckets recombine into
// pricing.Tokens' cache-inclusive Input before quoting.
func estimateSessionCost(rows []sessionstats.AggregatedRow, prices []pricing.ModelPrice) (costNanos int64, unpriced int64) {
	book := map[string]pricing.ModelPrice{}
	for _, price := range prices {
		book[price.Model] = price
	}
	for _, row := range rows {
		tokens := row.Input + row.Output + row.CacheRead + row.CacheWrite + row.Reasoning
		if tokens == 0 {
			continue
		}
		price, ok := book[row.Model]
		if !ok {
			unpriced += tokens
			continue
		}
		// Tiers are per-request assertions (prompt thresholds, service lanes,
		// time windows); a 15-minute aggregate cannot decide them, so the
		// estimate quotes base rates only (spec §5) — a tiered row must not
		// leak its long-context rate onto a whole bucket.
		price.Tiers = nil
		breakdown, err := pricing.Quote(price, 1.0, pricing.Tokens{
			Input:      row.Input + row.CacheRead + row.CacheWrite,
			Output:     row.Output + row.Reasoning,
			CacheRead:  row.CacheRead,
			CacheWrite: row.CacheWrite,
		}, row.Bucket15MS, "")
		if err != nil {
			unpriced += tokens
			continue
		}
		costNanos += breakdown.TotalNanos
	}
	return costNanos, unpriced
}

func (h *Handler) getSessionsSummary(writer http.ResponseWriter, request *http.Request) {
	reader, ok := h.sessionsReader(writer, request)
	if !ok {
		return
	}
	zone, zoneErr := sessionsTimezone(request)
	if zoneErr != "" {
		writeError(writer, http.StatusBadRequest, zoneErr)
		return
	}
	days, daysErr := sessionsDays(request)
	if daysErr != "" {
		writeError(writer, http.StatusBadRequest, daysErr)
		return
	}
	fromBucket := sessionstats.Bucket15MS(time.Now().AddDate(0, 0, -days).UnixMilli())
	rows, err := reader.SummaryRows(request.Context(), fromBucket)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	var prices []pricing.ModelPrice
	if h.pricing != nil {
		if book, err := h.pricing.ListPrices(request.Context()); err == nil {
			prices = book
		}
	}
	summary := sessionsSummaryDTO{CostSource: "estimated"}
	var totals sessionsTotals
	activeDays := map[string]struct{}{}
	weekday := map[string]int64{}
	dayTotals := map[string]int64{}
	type modelKey struct{ model, client string }
	models := map[modelKey]*sessionsTotals{}
	modelRows := map[modelKey][]sessionstats.AggregatedRow{}
	for _, row := range rows {
		totals.add(row)
		rowTotal := row.Input + row.Output + row.CacheRead + row.CacheWrite + row.Reasoning
		date := dayKey(row.Bucket15MS, zone)
		activeDays[date] = struct{}{}
		dayTotals[date] += rowTotal
		weekday[time.UnixMilli(row.Bucket15MS).In(zone).Format("Mon")] += rowTotal
		key := modelKey{model: row.Model, client: row.Client}
		if models[key] == nil {
			models[key] = &sessionsTotals{}
		}
		models[key].add(row)
		modelRows[key] = append(modelRows[key], row)
	}
	summary.Window = sessionTokenMixDTO(totals)
	summary.TotalTokens = totals.total()
	summary.ActiveDays = int64(len(activeDays))
	summary.Weekday = weekday
	costNanos, unpriced := estimateSessionCost(rows, prices)
	summary.CostUSD = float64(costNanos) / 1e9
	summary.UnpricedTokens = unpriced
	for key, modelTotals := range models {
		row := sessionModelRowDTO{Model: key.model, Clients: key.client, Tokens: modelTotals.total()}
		row.Share = shareOf(row.Tokens, summary.TotalTokens)
		row.Priced = pricedModel(row.Model, prices)
		if nanos, _ := estimateSessionCost(modelRows[key], prices); nanos > 0 {
			row.CostUSD = float64(nanos) / 1e9
		}
		summary.Models = append(summary.Models, row)
	}
	sortSessionModels(summary.Models)
	biggest, biggestTotal := "", int64(0)
	for date, total := range dayTotals {
		if total > biggestTotal {
			biggest, biggestTotal = date, total
		}
	}
	summary.BiggestDay = biggest
	sessionCounts, _ := reader.BucketSessionCounts(request.Context(), fromBucket)
	perDaySessions := foldSessionCounts(sessionCounts, zone)
	today := time.Now().In(zone).Format("2006-01-02")
	if day := foldDaily(rows, zone)[today]; day != nil {
		day.Sessions = perDaySessions[today]
		if nanos, _ := estimateSessionCost(rowsForDay(rows, zone, today), prices); nanos > 0 {
			day.CostUSD = float64(nanos) / 1e9
		}
		summary.Today = day
	}
	summary.Sessions = distinctWindowSessions(request.Context(), h, days)
	writeJSON(writer, http.StatusOK, summary)
}

func rowsForDay(rows []sessionstats.AggregatedRow, zone *time.Location, date string) []sessionstats.AggregatedRow {
	var filtered []sessionstats.AggregatedRow
	for _, row := range rows {
		if dayKey(row.Bucket15MS, zone) == date {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

// foldSessionCounts folds per-bucket distinct session counts into local days.
// A session crossing midnight counts in each civil day it touched (spec §4).
func foldSessionCounts(counts map[int64]int64, zone *time.Location) map[string]int64 {
	days := map[string]int64{}
	for bucket, count := range counts {
		days[dayKey(bucket, zone)] += count
	}
	return days
}

func (h *Handler) getSessionsDaily(writer http.ResponseWriter, request *http.Request) {
	reader, ok := h.sessionsReader(writer, request)
	if !ok {
		return
	}
	zone, zoneErr := sessionsTimezone(request)
	if zoneErr != "" {
		writeError(writer, http.StatusBadRequest, zoneErr)
		return
	}
	days, daysErr := sessionsDays(request)
	if daysErr != "" {
		writeError(writer, http.StatusBadRequest, daysErr)
		return
	}
	fromBucket := sessionstats.Bucket15MS(time.Now().AddDate(0, 0, -days).UnixMilli())
	rows, err := reader.SummaryRows(request.Context(), fromBucket)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	sessionCounts, err := reader.BucketSessionCounts(request.Context(), fromBucket)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	perDaySessions := foldSessionCounts(sessionCounts, zone)
	response := sessionsDailyDTO{}
	for date, day := range foldDaily(rows, zone) {
		day.Date = date
		day.Sessions = perDaySessions[date]
		response.Days = append(response.Days, *day)
	}
	sortSessionDays(response.Days)
	writeJSON(writer, http.StatusOK, response)
}

// getSessionsModels is the model table's own read: per-model tokens, share
// and estimated cost across the window.
func (h *Handler) getSessionsModels(writer http.ResponseWriter, request *http.Request) {
	reader, ok := h.sessionsReader(writer, request)
	if !ok {
		return
	}
	days, daysErr := sessionsDays(request)
	if daysErr != "" {
		writeError(writer, http.StatusBadRequest, daysErr)
		return
	}
	fromBucket := sessionstats.Bucket15MS(time.Now().AddDate(0, 0, -days).UnixMilli())
	rows, err := reader.SummaryRows(request.Context(), fromBucket)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	var prices []pricing.ModelPrice
	if h.pricing != nil {
		if book, err := h.pricing.ListPrices(request.Context()); err == nil {
			prices = book
		}
	}
	type modelKey struct{ model, client string }
	models := map[modelKey]*sessionsTotals{}
	modelRows := map[modelKey][]sessionstats.AggregatedRow{}
	var total int64
	for _, row := range rows {
		total += row.Input + row.Output + row.CacheRead + row.CacheWrite + row.Reasoning
		key := modelKey{model: row.Model, client: row.Client}
		if models[key] == nil {
			models[key] = &sessionsTotals{}
		}
		models[key].add(row)
		modelRows[key] = append(modelRows[key], row)
	}
	response := struct {
		Models []sessionModelRowDTO `json:"models"`
	}{}
	for key, modelTotals := range models {
		row := sessionModelRowDTO{Model: key.model, Clients: key.client, Tokens: modelTotals.total()}
		row.Share = shareOf(row.Tokens, total)
		row.Priced = pricedModel(row.Model, prices)
		if nanos, _ := estimateSessionCost(modelRows[key], prices); nanos > 0 {
			row.CostUSD = float64(nanos) / 1e9
		}
		response.Models = append(response.Models, row)
	}
	sortSessionModels(response.Models)
	writeJSON(writer, http.StatusOK, response)
}

func (h *Handler) getSessionsClients(writer http.ResponseWriter, request *http.Request) {
	reader, ok := h.sessionsReader(writer, request)
	if !ok {
		return
	}
	statuses, err := reader.ClientStatusRows(request.Context())
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	// Token and session totals come from the whole stored history, not a
	// window: the cards describe the client's coverage, the tiles the window.
	rows, err := reader.SummaryRows(request.Context(), 0)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	clientTokens := map[string]int64{}
	for _, row := range rows {
		clientTokens[row.Client] += row.Input + row.Output + row.CacheRead + row.CacheWrite + row.Reasoning
	}
	clientSessions, err := h.repo.ClientSessionCounts(request.Context())
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	response := sessionsClientsDTO{}
	for _, status := range statuses {
		response.Clients = append(response.Clients, sessionClientRowDTO{
			Client: status.Client, Tokens: clientTokens[status.Client],
			Sessions: clientSessions[status.Client], Entries: status.Entries,
			Files: status.Files, Unreadable: status.Unreadable, Oversized: status.Oversized,
			LastScanMS: status.LastScanMS,
		})
	}
	writeJSON(writer, http.StatusOK, response)
}

func (h *Handler) triggerSessionsRescan(writer http.ResponseWriter, request *http.Request) {
	if h.sessionScanner == nil {
		writeError(writer, http.StatusServiceUnavailable, "session statistics are not available")
		return
	}
	if h.cfg.IsDemoMode {
		writeError(writer, http.StatusForbidden, "session rescans need the local filesystem and are unavailable in the demonstration")
		return
	}
	if !h.sessionScanner.TriggerScan() {
		writeError(writer, http.StatusConflict, "a session scan is already pending")
		return
	}
	if err := h.recordAudit(request, "sessions.rescan.start", "sessions", "local", "success", nil); err != nil {
		writeInternalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"started": true})
}

func distinctWindowSessions(ctx context.Context, h *Handler, days int) int64 {
	if h.repo == nil {
		return 0
	}
	// Same edge as the token window (now − days) so the sessions tile and its
	// neighbouring tiles describe one window.
	from := time.Now().AddDate(0, 0, -days)
	fromBucket := sessionstats.Bucket15MS(from.UnixMilli())
	row := h.repo.SQL().QueryRowContext(ctx, `SELECT COUNT(DISTINCT client || ':' || session_id)
	 FROM session_activity WHERE bucket_15m >= ?`, fromBucket)
	var count int64
	if row.Scan(&count) != nil {
		return 0
	}
	return count
}

func sortSessionModels(rows []sessionModelRowDTO) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && (rows[j].Tokens > rows[j-1].Tokens || (rows[j].Tokens == rows[j-1].Tokens && rows[j].Model < rows[j-1].Model)); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

func sortSessionDays(days []sessionDayDTO) {
	for i := 1; i < len(days); i++ {
		for j := i; j > 0 && days[j].Date < days[j-1].Date; j-- {
			days[j], days[j-1] = days[j-1], days[j]
		}
	}
}

// pricedModel reports a book match regardless of the quoted amount: a free
// model prices at $0 and is still priced, not unpriced.
func pricedModel(model string, prices []pricing.ModelPrice) bool {
	for _, price := range prices {
		if price.Model == model {
			return true
		}
	}
	return false
}

func shareOf(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total)
}

// Compile-time assertions tying the repository reader interface together.
var _ SessionStatsReader = (*repository.Repository)(nil)
