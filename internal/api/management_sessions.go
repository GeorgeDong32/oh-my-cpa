package api

// Session statistics read APIs: client-side coding session usage in DTOs
// resolved against the viewer's calendar (docs/plans/session-stats.md §6).
// Reads are not audited (console precedent); rescan is audited and refused
// in demo mode like every write that needs a live filesystem.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
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

// SessionStatsScanner triggers the background scan loop; RefreshNow runs
// one synchronously (draining an in-flight scan first).
type SessionStatsScanner interface {
	TriggerScan() bool
	RefreshNow(ctx context.Context)
	IsScanning() bool
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

// sessionsRange is the window a sessions read covers: a rolling day count,
// the viewer's calendar today, or the whole stored history.
type sessionsRange struct {
	days  int  // rolling window; 0 means not a rolling range
	today bool // from the start of the viewer's local day
	all   bool // no lower bound
}

func sessionsRangeOf(request *http.Request) (sessionsRange, string) {
	raw := strings.TrimSpace(request.URL.Query().Get("days"))
	switch raw {
	case "", "30":
		return sessionsRange{days: 30}, ""
	case "today":
		return sessionsRange{today: true}, ""
	case "all":
		return sessionsRange{all: true}, ""
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 || days > sessionsMaxDays {
		return sessionsRange{}, "days must be an integer between 1 and 365, or today, or all"
	}
	return sessionsRange{days: days}, ""
}

// fromBucket is the window's lower bound on the 15-minute grid; 0 is unbounded.
func (r sessionsRange) fromBucket(now time.Time, zone *time.Location) int64 {
	switch {
	case r.all:
		return 0
	case r.today:
		// The viewer's calendar day start — Truncate would anchor on UTC.
		local := time.Now().In(zone)
		start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
		return sessionstats.Bucket15MS(start.UnixMilli())
	default:
		return sessionstats.Bucket15MS(now.AddDate(0, 0, -r.days).UnixMilli())
	}
}

// windowLabel is the days parameter the demo dataset and older callers use.
func (r sessionsRange) windowLabel() string {
	switch {
	case r.today:
		return "today"
	case r.all:
		return "all"
	default:
		return strconv.Itoa(r.days)
	}
}

func sessionsDays(request *http.Request) (int, string) {
	window, errText := sessionsRangeOf(request)
	if errText != "" {
		return 0, errText
	}
	if window.today || window.all {
		return 0, "days must be an integer between 1 and 365"
	}
	return window.days, ""
}

type sessionsTotals struct {
	Input, Output, CacheRead, CacheWrite, Reasoning int64
	Messages                                        int64
}

func (t *sessionsTotals) add(row sessionstats.AggregatedRow) {
	t.Input += row.Input
	t.Output += row.Output
	t.CacheRead += row.CacheRead
	t.CacheWrite += row.CacheWrite
	t.Reasoning += row.Reasoning
	t.Messages += row.Messages
}

func (t sessionsTotals) total() int64 {
	return t.Input + t.Output + t.CacheRead + t.CacheWrite + t.Reasoning
}

// dayKey is the local calendar date of a bucket under the viewer's zone.
func dayKey(bucketMS int64, zone *time.Location) string {
	return time.UnixMilli(bucketMS).In(zone).Format("2006-01-02")
}

// sessionsHourlyOf reads the chart's bucket granularity: days but an hour.
// It is only meaningful for the today range — a week of hourly columns is
// 168 bars of noise — so anything else is rejected rather than silently
// ignored.
func sessionsHourlyOf(request *http.Request, window sessionsRange) (bool, string) {
	switch strings.TrimSpace(request.URL.Query().Get("bucket")) {
	case "", "day":
		return false, ""
	case "hour":
		if !window.today {
			return false, "hourly buckets are only available for the today range"
		}
		return true, ""
	default:
		return false, "bucket must be day or hour"
	}
}

// bucketKey names a row's chart bucket: a calendar day, or for the today
// range's hourly read, a wall-clock hour in the viewer's zone.
func bucketKey(bucketMS int64, zone *time.Location, hourly bool) string {
	if hourly {
		local := time.UnixMilli(bucketMS).In(zone)
		hourStart := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, zone)
		return hourStart.Format("2006-01-02 15:04")
	}
	return dayKey(bucketMS, zone)
}

// foldDaily buckets the UTC grid rows into local days.
func foldDaily(rows []sessionstats.AggregatedRow, zone *time.Location, hourly bool) map[string]*sessionDayDTO {
	days := map[string]*sessionDayDTO{}
	for _, row := range rows {
		key := bucketKey(row.Bucket15MS, zone, hourly)
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
	Model    string  `json:"model"`
	Clients  string  `json:"clients"`
	Tokens   int64   `json:"tokens"`
	Share    float64 `json:"share"`
	Messages int64   `json:"messages"`
	CostUSD  float64 `json:"cost_usd"`
	Priced   bool    `json:"priced"`
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
//
// Rows are summed per model and quoted once per model: Quote is linear in
// every token bucket and the multiplier, so quoting the sum equals summing
// the quotes — and a whole-history read costs one Quote per model instead of
// one per cache row.
func estimateSessionCost(rows []sessionstats.AggregatedRow, prices []pricing.ModelPrice) (costNanos int64, unpriced int64) {
	book := newSessionBookIndex(prices)
	type modelTotals struct {
		input, output, cacheRead, cacheWrite int64
		tokens                               int64
		price                                pricing.ModelPrice
		priced                               bool
	}
	totals := map[string]*modelTotals{}
	for _, row := range rows {
		tokens := row.Input + row.Output + row.CacheRead + row.CacheWrite + row.Reasoning
		if tokens == 0 {
			continue
		}
		total := totals[row.Model]
		if total == nil {
			total = &modelTotals{}
			price, ok := book.lookup(row.Model)
			if ok {
				// Tiers are per-request assertions (prompt thresholds, service
				// lanes, time windows); an aggregate cannot decide them, so the
				// estimate quotes base rates only (spec §5).
				price.Tiers = nil
				total.price, total.priced = price, true
			}
			totals[row.Model] = total
		}
		// Quote's contract: Input is cache-inclusive — the quote itself
		// subtracts the cache fields before applying the prompt rate — and
		// Reasoning rides the completion channel.
		total.input += row.Input + row.CacheRead + row.CacheWrite
		total.output += row.Output + row.Reasoning
		// The unpriced share counts a model's tokens exactly once; it cannot
		// be read back out of the quote channels (input carries the cache
		// fields twice), so the token total gets its own accumulator.
		total.tokens += row.Input + row.Output + row.CacheRead + row.CacheWrite + row.Reasoning
		total.cacheRead += row.CacheRead
		total.cacheWrite += row.CacheWrite
	}
	for _, total := range totals {
		if !total.priced {
			unpriced += total.tokens
			continue
		}
		breakdown, err := pricing.Quote(total.price, 1.0, pricing.Tokens{
			Input: total.input, Output: total.output,
			CacheRead: total.cacheRead, CacheWrite: total.cacheWrite,
		}, 0, "")
		if err != nil {
			unpriced += total.tokens
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
	window, daysErr := sessionsRangeOf(request)
	if daysErr != "" {
		writeError(writer, http.StatusBadRequest, daysErr)
		return
	}
	zoneAny := zone
	fromBucket := window.fromBucket(time.Now(), zoneAny)
	rows, err := reader.SummaryRows(request.Context(), fromBucket)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	var prices []pricing.ModelPrice
	if h.pricing != nil {
		if book, err := h.pricing.ListKnownPrices(request.Context()); err == nil {
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
	summary.Window = sessionTokenMixDTO{Input: totals.Input, Output: totals.Output, CacheRead: totals.CacheRead, CacheWrite: totals.CacheWrite, Reasoning: totals.Reasoning}
	summary.TotalTokens = totals.total()
	summary.ActiveDays = int64(len(activeDays))
	summary.Weekday = weekday
	costNanos, unpriced := estimateSessionCost(rows, prices)
	summary.CostUSD = float64(costNanos) / 1e9
	summary.UnpricedTokens = unpriced
	for key, modelTotals := range models {
		row := sessionModelRowDTO{Model: key.model, Clients: key.client, Tokens: modelTotals.total(), Messages: modelTotals.Messages}
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
	perDaySessions := foldSessionCounts(sessionCounts, zone, false)
	today := time.Now().In(zone).Format("2006-01-02")
	if day := foldDaily(rows, zone, false)[today]; day != nil {
		day.Sessions = perDaySessions[today]
		if nanos, _ := estimateSessionCost(rowsForDay(rows, zone, today), prices); nanos > 0 {
			day.CostUSD = float64(nanos) / 1e9
		}
		summary.Today = day
	}
	summary.Sessions = distinctWindowSessionsRange(request.Context(), h, window, zone)
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
func foldSessionCounts(counts map[int64]int64, zone *time.Location, hourly bool) map[string]int64 {
	days := map[string]int64{}
	for bucket, count := range counts {
		days[bucketKey(bucket, zone, hourly)] += count
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
	window, daysErr := sessionsRangeOf(request)
	if daysErr != "" {
		writeError(writer, http.StatusBadRequest, daysErr)
		return
	}
	hourly, bucketErr := sessionsHourlyOf(request, window)
	if bucketErr != "" {
		writeError(writer, http.StatusBadRequest, bucketErr)
		return
	}
	fromBucket := window.fromBucket(time.Now(), zone)
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
	perDaySessions := foldSessionCounts(sessionCounts, zone, hourly)
	response := sessionsDailyDTO{}
	for date, day := range foldDaily(rows, zone, hourly) {
		day.Date = date
		day.Sessions = perDaySessions[date]
		response.Days = append(response.Days, *day)
	}
	sortSessionDays(response.Days)
	writeJSON(writer, http.StatusOK, response)
}

// getSessionsSeries answers the chart's split reads: per-day rows keyed by
// client or model, from the bucket cache. The cost stays a read-time
// estimate like every sessions number.
func (h *Handler) getSessionsSeries(writer http.ResponseWriter, request *http.Request) {
	if h.repo == nil {
		writeError(writer, http.StatusServiceUnavailable, "database is unavailable")
		return
	}
	zone, zoneErr := sessionsTimezone(request)
	if zoneErr != "" {
		writeError(writer, http.StatusBadRequest, zoneErr)
		return
	}
	window, daysErr := sessionsRangeOf(request)
	if daysErr != "" {
		writeError(writer, http.StatusBadRequest, daysErr)
		return
	}
	split := strings.TrimSpace(request.URL.Query().Get("split"))
	if split != "client" && split != "model" {
		writeError(writer, http.StatusBadRequest, "split must be client or model")
		return
	}
	hourly, bucketErr := sessionsHourlyOf(request, window)
	if bucketErr != "" {
		writeError(writer, http.StatusBadRequest, bucketErr)
		return
	}
	fromBucket := window.fromBucket(time.Now(), zone)
	rows, err := h.repo.BucketSeriesRows(request.Context(), fromBucket, split)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	var prices []pricing.ModelPrice
	if h.pricing != nil {
		if book, err := h.pricing.ListKnownPrices(request.Context()); err == nil {
			prices = book
		}
	}
	type seriesKey struct {
		date string
		name string
	}
	grouped := map[seriesKey]*sessionDayDTO{}
	for _, row := range rows {
		date := bucketKey(row.Bucket15MS, zone, hourly)
		name := row.Client
		if split == "model" {
			name = row.Model
		}
		day := grouped[seriesKey{date, name}]
		if day == nil {
			day = &sessionDayDTO{Date: date}
			grouped[seriesKey{date, name}] = day
		}
		day.Input += row.Input
		day.Output += row.Output
		day.CacheRead += row.CacheRead
		day.CacheWrite += row.CacheWrite
		day.Reasoning += row.Reasoning
		day.Messages += row.Messages
	}
	type seriesRow struct {
		Date       string  `json:"date"`
		Name       string  `json:"name"`
		Tokens     int64   `json:"tokens"`
		Input      int64   `json:"input"`
		Output     int64   `json:"output"`
		CacheRead  int64   `json:"cache_read"`
		CacheWrite int64   `json:"cache_write"`
		Reasoning  int64   `json:"reasoning"`
		CostUSD    float64 `json:"cost_usd"`
	}
	response := struct {
		Series []seriesRow `json:"series"`
	}{}
	keys := make([]seriesKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].date != keys[j].date {
			return keys[i].date < keys[j].date
		}
		return keys[i].name < keys[j].name
	})
	for _, key := range keys {
		day := grouped[key]
		row := seriesRow{Date: key.date, Name: key.name,
			Input: day.Input, Output: day.Output, CacheRead: day.CacheRead,
			CacheWrite: day.CacheWrite, Reasoning: day.Reasoning,
		}
		row.Tokens = row.Input + row.Output + row.CacheRead + row.CacheWrite + row.Reasoning
		if nanos, _ := estimateSessionCost(rowsForSeries(rows, split, key.name, key.date, zone, hourly), prices); nanos > 0 {
			row.CostUSD = float64(nanos) / 1e9
		}
		response.Series = append(response.Series, row)
	}
	writeJSON(writer, http.StatusOK, response)
}

func rowsForSeries(rows []sessionstats.AggregatedRow, split, name, date string, zone *time.Location, hourly bool) []sessionstats.AggregatedRow {
	var filtered []sessionstats.AggregatedRow
	for _, row := range rows {
		if bucketKey(row.Bucket15MS, zone, hourly) != date {
			continue
		}
		if (split == "client" && row.Client == name) || (split == "model" && row.Model == name) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

// getSessionsModels is the model table's own read: per-model tokens, share
// and estimated cost across the window.
func (h *Handler) getSessionsModels(writer http.ResponseWriter, request *http.Request) {
	reader, ok := h.sessionsReader(writer, request)
	if !ok {
		return
	}
	zone, zoneErr := sessionsTimezone(request)
	if zoneErr != "" {
		writeError(writer, http.StatusBadRequest, zoneErr)
		return
	}
	window, daysErr := sessionsRangeOf(request)
	if daysErr != "" {
		writeError(writer, http.StatusBadRequest, daysErr)
		return
	}
	fromBucket := window.fromBucket(time.Now(), zone)
	rows, err := reader.SummaryRows(request.Context(), fromBucket)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	var prices []pricing.ModelPrice
	if h.pricing != nil {
		if book, err := h.pricing.ListKnownPrices(request.Context()); err == nil {
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
		row := sessionModelRowDTO{Model: key.model, Clients: key.client, Tokens: modelTotals.total(), Messages: modelTotals.Messages}
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

// postSessionsRefresh is the manual refresh: a synchronous incremental scan.
// It drains an in-flight scan first and returns once the store is at least as
// fresh as the call — the page reloads its queries on 200 without polling.
func (h *Handler) postSessionsRefresh(writer http.ResponseWriter, request *http.Request) {
	if h.sessionScanner == nil {
		writeError(writer, http.StatusServiceUnavailable, "session statistics are not available")
		return
	}
	if h.cfg.IsDemoMode {
		writeError(writer, http.StatusForbidden, "session refreshes need the local filesystem and are unavailable in the demonstration")
		return
	}
	wasScanning := h.sessionScanner.IsScanning()
	h.sessionScanner.RefreshNow(request.Context())
	if err := h.recordAudit(request, "sessions.refresh", "sessions", "local", "success", map[string]any{"drained_in_flight": wasScanning}); err != nil {
		writeInternalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"refreshed": true})
}

func distinctWindowSessionsRange(ctx context.Context, h *Handler, window sessionsRange, zone *time.Location) int64 {
	if h.repo == nil {
		return 0
	}
	// Same edge as the token window so the sessions tile and its neighbouring
	// tiles describe one window.
	fromBucket := window.fromBucket(time.Now(), zone)
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
	_, ok := newSessionBookIndex(prices).lookup(model)
	return ok
}

// sessionBookIndex answers the sessions estimator's lookups. The names in
// session files are the clients' own spellings ("Minimax/MiniMax-M3",
// "gpt-5.6-sol", "bailian/deepseek-v4-flash-0731"), while the price book
// stores the gateway's catalog names ("MiniMax-M3", "codex/gpt-5.6-sol") —
// an exact-only match calls the same model unpriced because two namespaces
// disagree on spelling. The index therefore also keys a normalised form:
// lowercased, channel/author prefix dropped, trailing six-digit snapshot
// date removed, matched against both the row's name and its upstream id.
// A name with no row under any of these — gpt-5.5 has no book row at all —
// stays unpriced rather than borrowing a neighbour's rates.
type sessionBookIndex struct {
	exact map[string]pricing.ModelPrice
	slug  map[string]pricing.ModelPrice
}

func newSessionBookIndex(prices []pricing.ModelPrice) sessionBookIndex {
	index := sessionBookIndex{exact: map[string]pricing.ModelPrice{}, slug: map[string]pricing.ModelPrice{}}
	for _, price := range prices {
		index.exact[price.Model] = price
		for _, key := range []string{price.Model, price.UpstreamID} {
			if key == "" {
				continue
			}
			s := sessionModelSlug(key)
			if _, exists := index.slug[s]; !exists {
				index.slug[s] = price
			}
		}
	}
	return index
}

func (i sessionBookIndex) lookup(model string) (pricing.ModelPrice, bool) {
	if price, ok := i.exact[model]; ok {
		return price, true
	}
	price, ok := i.slug[sessionModelSlug(model)]
	return price, ok
}

// sessionModelSlug normalises one model identity for the fallback match.
func sessionModelSlug(model string) string {
	slug := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(slug, '/'); i >= 0 {
		slug = slug[i+1:]
	}
	if stripped, ok := stripDateSnapshot(slug); ok {
		slug = stripped
	}
	return slug
}

// stripDateSnapshot removes a trailing date-snapshot suffix ("…-0731",
// "…-2507", "…-260428", "…-20240620"): providers re-publish the same model
// under dated names at the same rates. The digits must sit behind a dash and
// decode to a month at one of the usual positions, so "qwen3-32b" survives.
func stripDateSnapshot(slug string) (string, bool) {
	i := strings.LastIndexByte(slug, '-')
	if i <= 0 {
		return slug, false
	}
	tail := slug[i+1:]
	if len(tail) < 4 || len(tail) > 8 || (len(tail)%2) != 0 {
		return slug, false
	}
	for _, r := range tail {
		if r < '0' || r > '9' {
			return slug, false
		}
	}
	monthOK := func(pair string) bool { return pair >= "01" && pair <= "12" }
	dateLike := false
	switch len(tail) {
	case 4: // MMDD or YYMM
		dateLike = monthOK(tail[:2]) || monthOK(tail[2:])
	case 6: // YYMMDD
		dateLike = monthOK(tail[2:4])
	case 8: // YYYYMMDD
		dateLike = monthOK(tail[4:6])
	}
	if !dateLike {
		return slug, false
	}
	return slug[:i], true
}

func shareOf(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total)
}

// Compile-time assertions tying the repository reader interface together.
var _ SessionStatsReader = (*repository.Repository)(nil)
