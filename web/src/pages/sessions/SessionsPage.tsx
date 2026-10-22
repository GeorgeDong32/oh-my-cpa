import React from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Segmented } from 'antd';
import { api } from '../../api/client';
import { useT } from '../../i18n';
import { useTimeZone } from '../../utils/TimeZoneProvider';
import { PageHeader } from '../../components/common/PageHeader';
import { RefreshButton } from '../../components/common/RefreshButton';
import { LoadFailure, Notice } from '../../components/feedback';
import { formatTokens } from '../../types/tokenDisplay';
import { useTokenDisplayStyle } from '../../types/tokenDisplayContext';
import {
  SESSIONS_RANGE_PREFERENCE,
  DEFAULT_SESSIONS_RANGE,
  parseSessionsRange,
  sessionsRangeParam,
  type SessionsRange,
  type SessionsSeriesMode,
  DEFAULT_SESSIONS_SERIES_MODE,
} from '../../types/sessionsRange';
import { usePreference } from '../../hooks/usePreference';
import { SessionsTrendChart } from '../../charts/SessionsTrendChart';
import { ModelMark } from '../../components/ModelMark';
import { ClientMark } from './ClientMark';
import type { SessionDay, SessionModelRow, SessionsSeriesPoint, SessionTokenMix } from '../../types/sessions';
import './sessions.css';

const SESSIONS_QUERY_KEYS = {
  all: ['sessions'] as const,
};

const SERIES_SLOTS = 6;

function mixTotal(day: Pick<SessionDay, 'input' | 'output' | 'cache_read' | 'cache_write' | 'reasoning'>): number {
  return day.input + day.output + day.cache_read + day.cache_write + day.reasoning;
}

function formatUsd(value: number): string {
  return `$${value.toFixed(2)}`;
}

const MIX_PARTS: Array<{ className: string; key: keyof SessionTokenMix }> = [
  { className: 'comp-input', key: 'input' },
  { className: 'comp-output', key: 'output' },
  { className: 'comp-cache-read', key: 'cache_read' },
  { className: 'comp-cache-write', key: 'cache_write' },
  { className: 'comp-reasoning', key: 'reasoning' },
];

const TokenCompositionBar: React.FC<{ mix: SessionTokenMix }> = ({ mix }) => {
  const total = mixTotal(mix);
  if (total <= 0) return <div className="sessions-comp" aria-hidden />;
  return (
    <div className="sessions-comp" data-testid="sessions-composition">
      {MIX_PARTS.map((part) => (
        mix[part.key] > 0 ? <div key={part.className} className={`sessions-comp-part ${part.className}`} style={{ flexGrow: mix[part.key] }} /> : null
      ))}
    </div>
  );
};

/** tokens.ci's bucket legend: a colour dot, the bucket name, and its count. */
const MixLegend: React.FC<{ mix: SessionTokenMix }> = ({ mix }) => {
  const t = useT();
  const { style } = useTokenDisplayStyle();
  const fmtTokens = React.useCallback((tokens: number) => formatTokens(tokens, style), [style]);
  const labels: Record<keyof SessionTokenMix, string> = {
    input: t('sessions.mix.input'),
    output: t('sessions.mix.output'),
    cache_read: t('sessions.mix.cache_read'),
    cache_write: t('sessions.mix.cache_write'),
    reasoning: t('sessions.mix.reasoning'),
  };
  return (
    <div className="sessions-legend">
      {MIX_PARTS.map((part) => (
        <span key={part.className} className="sessions-legend-item">
          <span className={`sessions-legend-swatch ${part.className}`} aria-hidden />
          <span className="sessions-legend-label">{labels[part.key]}</span>
          <span className="sessions-legend-value mono">{fmtTokens(mix[part.key])}</span>
        </span>
      ))}
    </div>
  );
};

/**
 * ClientBreakdown renders tokens.ci's two-level client cards: one row per
 * client with its message count, cost, and token total; beneath it one row
 * per model with a share bar and the model's token count. Rows come from the
 * window's per-(client, model) summary rows.
 */
const ClientBreakdown: React.FC<{ models?: SessionModelRow[] }> = ({ models }) => {
  const t = useT();
  const { style } = useTokenDisplayStyle();
  const fmtTokens = React.useCallback((tokens: number) => formatTokens(tokens, style), [style]);
  const groups = React.useMemo(() => {
    const byClient = new Map<string, SessionModelRow[]>();
    for (const row of models ?? []) {
      const list = byClient.get(row.clients) ?? [];
      list.push(row);
      byClient.set(row.clients, list);
    }
    return [...byClient.entries()]
      .map(([client, rows]) => {
        const sorted = rows.slice().sort((left, right) => right.tokens - left.tokens);
        return {
          client,
          rows: sorted,
          tokens: sorted.reduce((sum, row) => sum + row.tokens, 0),
          messages: sorted.reduce((sum, row) => sum + row.messages, 0),
          costUsd: sorted.reduce((sum, row) => sum + row.cost_usd, 0),
          allPriced: sorted.every((row) => row.priced),
        };
      })
      .sort((left, right) => right.tokens - left.tokens);
  }, [models]);
  if (groups.length === 0) return null;
  return (
    <div className="sessions-break" data-testid="sessions-breakdown">
      {groups.map((group) => (
        <div key={group.client} className="sessions-break-client">
          <div className="sessions-break-client-head">
            <span className="sessions-break-client-name"><ClientMark client={group.client} />{group.client}</span>
            <span className="sessions-break-client-meta">
              <span>{group.messages} {t('sessions.messages')}</span>
              <span>{group.allPriced ? formatUsd(group.costUsd) : '—'}</span>
              <span className="sessions-break-client-tokens mono">{fmtTokens(group.tokens)}</span>
            </span>
          </div>
          {group.rows.map((row) => {
            const share = group.tokens > 0 ? row.tokens / group.tokens : 0;
            return (
              <div key={row.model} className="sessions-break-model">
                <span className="sessions-break-model-name mono" title={row.model}><ModelMark model={row.model} size={13} />{row.model}</span>
                <span className="sessions-break-model-bar" aria-hidden>
                  <span className="sessions-break-model-fill" style={{ width: `${Math.max(share * 100, 1.5)}%` }} />
                </span>
                <span className="sessions-break-model-pct mono">{(share * 100).toFixed(0)}%</span>
                <span className="sessions-break-model-tokens mono">{fmtTokens(row.tokens)}</span>
              </div>
            );
          })}
        </div>
      ))}
    </div>
  );
};

export const SessionsPage: React.FC = () => {
  const t = useT();
  const { style } = useTokenDisplayStyle();
  const fmtTokens = React.useCallback((tokens: number) => formatTokens(tokens, style), [style]);
  const tz = useTimeZone() || 'UTC';
  const queryClient = useQueryClient();
  const rangePref = usePreference<SessionsRange>(SESSIONS_RANGE_PREFERENCE, DEFAULT_SESSIONS_RANGE, parseSessionsRange);
  const range = rangePref.value;
  const [seriesMode, setSeriesMode] = React.useState<SessionsSeriesMode>(DEFAULT_SESSIONS_SERIES_MODE);
  const [refreshing, setRefreshing] = React.useState(false);
  const rangeParam = sessionsRangeParam(range);

  const summary = useQuery({
    queryKey: [...SESSIONS_QUERY_KEYS.all, 'summary', rangeParam, tz],
    queryFn: () => api.getSessionsSummary(rangeParam, tz),
  });
  const series = useQuery({
    queryKey: [...SESSIONS_QUERY_KEYS.all, 'series', rangeParam, seriesMode, tz],
    queryFn: () =>
      seriesMode === 'total'
        ? api.getSessionsDaily(rangeParam, tz).then((daily): SessionsSeriesPoint[] =>
            (daily.days ?? []).map((day) => ({
              date: day.date,
              series: t('sessions.chart.total'),
              tokens: mixTotal(day),
            })),
          )
        : api
            .getSessionsSeries(rangeParam, seriesMode, tz)
            .then((result): SessionsSeriesPoint[] => {
              const rows = (result.series ?? []).map((row) => ({
                date: row.date,
                series: row.name,
                tokens: row.tokens,
              }));
              // Fold the long tail into one grey remainder: past the series
              // slots the hue wheel repeats, and a dozen pale stacks read as
              // noise rather than as ranking.
              const totals = new Map<string, number>();
              for (const row of rows) totals.set(row.series, (totals.get(row.series) ?? 0) + row.tokens);
              const ranked = [...totals.entries()].sort((left, right) => right[1] - left[1]);
              const keep = new Set(ranked.slice(0, SERIES_SLOTS).map(([name]) => name));
              return rows.map((row) => ({
                ...row,
                series: keep.has(row.series) ? row.series : `${t('sessions.chart.other')} (${ranked.length - SERIES_SLOTS})`,
              }));
            }),
  });
  const clients = useQuery({
    queryKey: [...SESSIONS_QUERY_KEYS.all, 'clients'],
    queryFn: () => api.getSessionsClients(),
  });

  // A store that has never completed a scan answers every read with zeros:
  // the first scan is still walking the sources. Poll until it lands instead
  // of showing an empty page.
  const firstScanPending = (clients.data?.clients ?? []).length > 0 &&
    (clients.data?.clients ?? []).every((client) => client.last_scan_ms === 0);
  React.useEffect(() => {
    if (!firstScanPending) return;
    // Re-read on the poll cadence until the first scan lands; the queries
    // re-arm the interval through firstScanPending flipping false.
    const timer = window.setInterval(() => {
      void queryClient.invalidateQueries({ queryKey: SESSIONS_QUERY_KEYS.all });
    }, 5000);
    return () => window.clearInterval(timer);
  }, [firstScanPending, queryClient]);

  const invalidate = React.useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: SESSIONS_QUERY_KEYS.all });
  }, [queryClient]);

  const manualRefresh = React.useCallback(async () => {
    setRefreshing(true);
    try {
      await api.refreshSessions();
    } finally {
      setRefreshing(false);
      invalidate();
    }
  }, [invalidate]);

  const data = summary.data;
  const mix = data?.tokens;
  // The today card reads the summary itself: on the today range the window is
  // the viewer's calendar day, so one capture answers both hero and card.
  const todayData = range === 'today' ? data : undefined;
  const todayMix = todayData?.tokens;

  const chartPoints = React.useMemo(() => {
    const rows = series.data ?? [];
    return rows;
  }, [series.data]);

  // Per-client window totals for the detected-clients grid: the headline
  // number follows the selected range; files/entries/last-scan stay lifetime
  // facts about what sits on disk.
  const windowByClient = React.useMemo(() => {
    const totals = new Map<string, { tokens: number; messages: number }>();
    for (const row of data?.models ?? []) {
      const current = totals.get(row.clients) ?? { tokens: 0, messages: 0 };
      current.tokens += row.tokens;
      current.messages += row.messages;
      totals.set(row.clients, current);
    }
    return totals;
  }, [data?.models]);

  const rangeOptions = [
    { label: t('sessions.range.today'), value: 'today' },
    { label: t('sessions.range.7d'), value: '7d' },
    { label: t('sessions.range.30d'), value: '30d' },
    { label: t('sessions.range.all'), value: 'all' },
  ];
  const modeOptions = [
    { label: t('sessions.chart.total'), value: 'total' },
    { label: t('sessions.chart.by_client'), value: 'client' },
    { label: t('sessions.chart.by_model'), value: 'model' },
  ];

  return (
    <div className="terminal-page sessions-page">
      <PageHeader
        title={t('sessions.title')}
        subtitle={t('sessions.caption')}
        actions={(
          <>
            <Segmented
              options={rangeOptions}
              value={range}
              onChange={(value) => rangePref.set(value as SessionsRange)}
              aria-label={t('sessions.range.label')}
            />
            <RefreshButton
              isRefreshing={refreshing || summary.isFetching || series.isFetching}
              onRefresh={() => void manualRefresh()}
              label={t('sessions.refresh')}
            />
          </>
        )}
      />
      {firstScanPending && <Notice tone="info" title={t('sessions.first_scan.title')} />}
      {summary.isError && <LoadFailure title={t('sessions.title')} onRetry={() => void summary.refetch()} />}
      {data && mix && (
        <section className="sessions-hero">
          <div className="sessions-stats">
            <div className="sessions-stat">
              <div className="sessions-stat-label">{t('sessions.total_tokens')}</div>
              <div className="sessions-stat-value mono">{fmtTokens(data.total_tokens)}</div>
            </div>
            <div className="sessions-stat">
              <div className="sessions-stat-label">{t('sessions.cost')}</div>
              <div className="sessions-stat-value mono">{formatUsd(data.cost_usd)}</div>
              <div className="sessions-stat-caption">
                {t('sessions.cost_note')}
                {data.unpriced_tokens > 0 && <> {t('sessions.unpriced', { tokens: fmtTokens(data.unpriced_tokens) })}</>}
              </div>
            </div>
            <div className="sessions-stat">
              <div className="sessions-stat-label">{t('sessions.active_days')}</div>
              <div className="sessions-stat-value mono">{data.active_days}</div>
              <div className="sessions-stat-caption">{t('sessions.active_days_note')}</div>
            </div>
            <div className="sessions-stat">
              <div className="sessions-stat-label">{t('sessions.sessions')}</div>
              <div className="sessions-stat-value mono">{data.sessions}</div>
              <div className="sessions-stat-caption">{t('sessions.sessions_note')}</div>
            </div>
          </div>
          <TokenCompositionBar mix={mix} />
          <MixLegend mix={mix} />
        </section>
      )}
      {todayMix && (
        <section className="sessions-today" data-testid="sessions-today">
          <div className="sessions-today-head">
            <div className="sessions-today-title">
              <h2>{t('sessions.today')}</h2>
              {todayData?.today?.date && <span className="sessions-today-date mono">{todayData.today.date}</span>}
            </div>
            <div className="sessions-today-headstats">
              <span className="sessions-today-headstat">
                <span className="sessions-today-headstat-label">{t('sessions.col.tokens')}</span>
                <span className="sessions-today-headstat-value mono">{fmtTokens(mixTotal(todayMix))}</span>
              </span>
              {todayData && (
                <span className="sessions-today-headstat">
                  <span className="sessions-today-headstat-label">{t('sessions.cost')}</span>
                  <span className="sessions-today-headstat-value mono">{formatUsd(todayData.cost_usd)}</span>
                </span>
              )}
              <span className="sessions-today-headstat">
                <span className="sessions-today-headstat-label">{t('sessions.messages')}</span>
                <span className="sessions-today-headstat-value mono">
                  {todayData?.today?.messages ?? todayData?.sessions ?? 0}
                </span>
              </span>
            </div>
          </div>
          <TokenCompositionBar mix={todayMix} />
          <MixLegend mix={todayMix} />
          <ClientBreakdown models={todayData?.models} />
        </section>
      )}
      {chartPoints.length > 0 && (
        <section className="sessions-section">
          <div className="sessions-chart-head">
            <h2>{t('sessions.daily')}</h2>
            <Segmented
              options={modeOptions}
              value={seriesMode}
              onChange={(value) => setSeriesMode(value as SessionsSeriesMode)}
              aria-label={t('sessions.chart.mode')}
            />
          </div>
          <div className="sessions-chart" data-testid="sessions-daily">
            {series.isError
              ? <LoadFailure title={t('sessions.daily')} onRetry={() => void series.refetch()} />
              : <SessionsTrendChart points={chartPoints} />}
          </div>
        </section>
      )}
      {/* The today range's breakdown IS the today card's; repeating it as a
          section would print the same cards twice on one screen. */}
      {range !== 'today' && data && data.models && data.models.length > 0 && (
        <section className="sessions-section">
          <h2>{t('sessions.breakdown')}</h2>
          <ClientBreakdown models={data.models} />
        </section>
      )}
      {range !== 'today' && clients.data && clients.data.clients && clients.data.clients.length > 0 && (
        <section className="sessions-section">
          <h2>{t('sessions.clients')}</h2>
          <div className="sessions-grid">
            {clients.data.clients.map((client) => {
              const stat = windowByClient.get(client.client);
              return (
                <div key={client.client} className="sessions-tile sessions-client">
                  <div className="sessions-tile-label mono sessions-client-label"><ClientMark client={client.client} />{client.client}</div>
                  <div className="sessions-tile-value">{fmtTokens(stat?.tokens ?? 0)}</div>
                  <div className="sessions-tile-caption">
                    {stat && <span>{t('sessions.messages')} {stat.messages}</span>}
                    <span>{t('sessions.client.files')} {client.files}</span>
                    <span>{t('sessions.client.entries')} {client.entries}</span>
                  </div>
                  {client.last_scan_ms > 0 && <div className="sessions-tile-caption">{t('sessions.client.last_scan')} {new Date(client.last_scan_ms).toLocaleString()}</div>}
                </div>
              );
            })}
          </div>
        </section>
      )}
      {!data && !summary.isError && summary.isLoading && <Notice tone="info" title={t('common.loading')} />}
    </div>
  );
};

export default SessionsPage;
