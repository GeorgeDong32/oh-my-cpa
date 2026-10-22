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
import type { SessionDay, SessionsSeriesPoint } from '../../types/sessions';
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

const TokenCompositionBar: React.FC<{ mix: NonNullable<import('../../types/sessions').SessionsSummary['tokens']> }> = ({ mix }) => {
  const parts = [
    { className: 'comp-input', value: mix.input },
    { className: 'comp-output', value: mix.output },
    { className: 'comp-cache-read', value: mix.cache_read },
    { className: 'comp-cache-write', value: mix.cache_write },
    { className: 'comp-reasoning', value: mix.reasoning },
  ];
  const total = mix.input + mix.output + mix.cache_read + mix.cache_write + mix.reasoning;
  if (total <= 0) return <div className="sessions-comp" aria-hidden />;
  return (
    <div className="sessions-comp" data-testid="sessions-composition">
      {parts.map((part) => (
        part.value > 0 ? <div key={part.className} className={`sessions-comp-part ${part.className}`} style={{ flexGrow: part.value }} /> : null
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
  // The Today strip is its own capture: it stays on the viewer's calendar day
  // no matter which history range the tiles above are reading.
  const today = useQuery({
    queryKey: [...SESSIONS_QUERY_KEYS.all, 'summary', 'today', tz],
    queryFn: () => api.getSessionsSummary('today', tz),
    enabled: range !== 'today',
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
  const todayData = range === 'today' ? data : today.data;
  const todayMix = todayData?.tokens;

  const chartPoints = React.useMemo(() => {
    const rows = series.data ?? [];
    return rows;
  }, [series.data]);

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
      {todayMix && range !== 'today' && (
        <section className="sessions-section sessions-today" data-testid="sessions-today">
          <div className="sessions-today-head">
            <h2>{t('sessions.today')}</h2>
            <span className="sessions-today-total">{fmtTokens(mixTotal(todayMix))}</span>
          </div>
          <TokenCompositionBar mix={todayMix} />
          <div className="sessions-today-caption">
            <span>{t('sessions.mix.input')} {fmtTokens(todayMix.input)}</span>
            <span>{t('sessions.mix.output')} {fmtTokens(todayMix.output)}</span>
            <span>{t('sessions.mix.cache_read')} {fmtTokens(todayMix.cache_read)}</span>
            <span>{t('sessions.mix.reasoning')} {fmtTokens(todayMix.reasoning)}</span>
            {todayData && todayData.sessions > 0 && <span>{t('sessions.sessions')} {todayData.sessions}</span>}
          </div>
        </section>
      )}
      {data && (
        <div className="sessions-grid">
          <div className="sessions-tile">
            <div className="sessions-tile-label">{t('sessions.total_tokens')}</div>
            <div className="sessions-tile-value">{fmtTokens(data.total_tokens)}</div>
            {mix && <TokenCompositionBar mix={mix} />}
          </div>
          <div className="sessions-tile">
            <div className="sessions-tile-label">{t('sessions.cost')}</div>
            <div className="sessions-tile-value">{formatUsd(data.cost_usd)}</div>
            <div className="sessions-tile-caption">{t('sessions.cost_note')}</div>
            {data.unpriced_tokens > 0 && (
              <div className="sessions-tile-caption">{t('sessions.unpriced', { tokens: fmtTokens(data.unpriced_tokens) })}</div>
            )}
          </div>
          <div className="sessions-tile">
            <div className="sessions-tile-label">{t('sessions.active_days')}</div>
            <div className="sessions-tile-value">{data.active_days}</div>
            <div className="sessions-tile-caption">{t('sessions.active_days_note')}</div>
          </div>
          <div className="sessions-tile">
            <div className="sessions-tile-label">{t('sessions.sessions')}</div>
            <div className="sessions-tile-value">{data.sessions}</div>
            <div className="sessions-tile-caption">{t('sessions.sessions_note')}</div>
          </div>
        </div>
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
          <p className="sessions-horizon">{t('sessions.horizon')}</p>
        </section>
      )}
      {data && data.models && data.models.length > 0 && (
        <section className="sessions-section">
          <h2>{t('sessions.models')}</h2>
          <div className="sessions-table-wrap">
            <table className="sessions-table">
              <thead>
                <tr>
                  <th>{t('sessions.col.model')}</th>
                  <th>{t('sessions.col.client')}</th>
                  <th className="num">{t('sessions.col.tokens')}</th>
                  <th className="num">{t('sessions.col.share')}</th>
                  <th className="num">{t('sessions.col.cost')}</th>
                </tr>
              </thead>
              <tbody>
                {data.models.map((model) => (
                  <tr key={`${model.model}/${model.clients}`}>
                    <td className="mono">{model.model}</td>
                    <td>{model.clients}</td>
                    <td className="num mono">{fmtTokens(model.tokens)}</td>
                    <td className="num mono">{(model.share * 100).toFixed(1)}%</td>
                    <td className="num mono">{model.priced ? formatUsd(model.cost_usd) : '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
      {clients.data && clients.data.clients && clients.data.clients.length > 0 && (
        <section className="sessions-section">
          <h2>{t('sessions.clients')}</h2>
          <div className="sessions-grid">
            {clients.data.clients.map((client) => (
              <div key={client.client} className="sessions-tile sessions-client">
                <div className="sessions-tile-label mono">{client.client}</div>
                <div className="sessions-tile-value">{fmtTokens(client.tokens)}</div>
                <div className="sessions-tile-caption">
                  <span>{t('sessions.client.files')} {client.files}</span>
                  <span>{t('sessions.client.entries')} {client.entries}</span>
                  {client.sessions > 0 && <span>{t('sessions.sessions')} {client.sessions}</span>}
                </div>
                {client.last_scan_ms > 0 && <div className="sessions-tile-caption">{t('sessions.client.last_scan')} {new Date(client.last_scan_ms).toLocaleString()}</div>}
              </div>
            ))}
          </div>
        </section>
      )}
      {!data && !summary.isError && summary.isLoading && <Notice tone="info" title={t('common.loading')} />}
    </div>
  );
};

export default SessionsPage;
