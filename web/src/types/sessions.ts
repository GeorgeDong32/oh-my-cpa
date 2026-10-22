/** Local session statistics: client-side coding session usage read from the
 * coding agents' own session files (docs/plans/session-stats.md). */

export interface SessionTokenMix {
  input: number;
  output: number;
  cache_read: number;
  cache_write: number;
  reasoning: number;
}

export interface SessionModelRow {
  model: string;
  clients: string;
  tokens: number;
  share: number;
  messages: number;
  cost_usd: number;
  priced: boolean;
}

export interface SessionsSummary {
  tokens: SessionTokenMix;
  total_tokens: number;
  cost_usd: number;
  cost_source: string;
  unpriced_tokens: number;
  active_days: number;
  sessions: number;
  today?: SessionDay;
  biggest_day?: string;
  weekday?: Record<string, number>;
  models?: SessionModelRow[];
}

export interface SessionDay {
  date: string;
  input: number;
  output: number;
  cache_read: number;
  cache_write: number;
  reasoning: number;
  messages: number;
  sessions: number;
  cost_usd: number;
}

export interface SessionsDaily {
  days: SessionDay[];
}

export interface SessionClientRow {
  client: string;
  tokens: number;
  sessions: number;
  entries: number;
  files: number;
  unreadable: number;
  oversized: number;
  last_scan_ms: number;
}

export interface SessionsModels {
  models?: SessionModelRow[];
}

export interface SessionsClients {
  clients?: SessionClientRow[];
}

/** One flat chart row: a calendar day, a series name (client or model, or the
 * folded remainder), and the day's total tokens for that series. */
export interface SessionsSeriesPoint {
  date: string;
  series: string;
  tokens: number;
  costUsd: number;
}

export interface SessionsSeriesRow {
  date: string;
  name: string;
  tokens: number;
  input: number;
  output: number;
  cache_read: number;
  cache_write: number;
  reasoning: number;
  cost_usd: number;
}

export interface SessionsSeries {
  series?: SessionsSeriesRow[];
}
