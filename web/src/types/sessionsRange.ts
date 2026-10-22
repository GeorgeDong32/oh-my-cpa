/** The sessions page's range. `today` anchors on the viewer's calendar day;
 * `all` reads the whole stored history. */
export type SessionsRange = 'today' | '7d' | '30d' | 'all';

export const DEFAULT_SESSIONS_RANGE: SessionsRange = '30d';
export const SESSIONS_RANGE_PREFERENCE = 'omc_sessions_range';

export function parseSessionsRange(raw: unknown): SessionsRange | undefined {
  return raw === 'today' || raw === '7d' || raw === '30d' || raw === 'all' ? raw : undefined;
}

/** The `days` query parameter each range sends; the backend accepts the words
 * beside rolling day counts. */
export function sessionsRangeParam(range: SessionsRange): string {
  switch (range) {
    case 'today': return 'today';
    case 'all': return 'all';
    case '7d': return '7';
    case '30d': return '30';
  }
}

/** Which dimension the daily chart's stacks colour by. */
export type SessionsSeriesMode = 'client' | 'model';

export const DEFAULT_SESSIONS_SERIES_MODE: SessionsSeriesMode = 'model';
