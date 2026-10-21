import React from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../api/client';
import { useT } from '../../i18n';
import { useTimeZone } from '../../utils/TimeZoneProvider';
import { PageHeader } from '../../components/common/PageHeader';
import { RefreshButton } from '../../components/common/RefreshButton';
import { LoadFailure, Notice } from '../../components/feedback';
import { formatTokens } from '../../types/tokenDisplay';
import { useTokenDisplayStyle } from '../../types/tokenDisplayContext';
import './sessions.css';
import type { SessionTokenMix } from '../../types/sessions';


const SESSIONS_QUERY_KEYS = {
  all: ['sessions'] as const,
};

// The API keys weekdays in the viewer locale's abbreviated English form; the
// bars render in the console language with a stable order.
const WEEKDAY_ORDER = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

function formatUsd(value: number): string {
  return `$${value.toFixed(2)}`;
}

function mixTotal(mix: SessionTokenMix): number {
  return mix.input + mix.output + mix.cache_read + mix.cache_write + mix.reasoning;
}

/** The five-part composition bar: each bucket's share of the window total. */
function TokenCompositionBar({ mix }: { mix: SessionTokenMix }) {
  const parts = [
    { className: 'comp-input', value: mix.input },
    { className: 'comp-output', value: mix.output },
    { className: 'comp-cache-read', value: mix.cache_read },
    { className: 'comp-cache-write', value: mix.cache_write },
    { className: 'comp-reasoning', value: mix.reasoning },
  ];
  const total = mixTotal(mix);
  if (total <= 0) return <div className="sessions-comp" aria-hidden />;
  return (
    <div className="sessions-comp" data-testid="sessions-composition">
      {parts.map((part) => (
        part.value > 0 ? <div key={part.className} className={`sessions-comp-part ${part.className}`} style={{ flexGrow: part.value }} /> : null
      ))}
    </div>
  );
}

export const SessionsPage: React.FC = () => {
  const t = useT();
  const { style } = useTokenDisplayStyle();
  const fmtTokens = React.useCallback((tokens: number) => formatTokens(tokens, style), [style]);
  const tz = useTimeZone() || 'UTC';
  const queryClient = useQueryClient();
  const [days, setDays] = React.useState(30);

  const summary = useQuery({
    queryKey: [...SESSIONS_QUERY_KEYS.all, 'summary', days, tz],
    queryFn: () => api.getSessionsSummary(days, tz),
  });
  const daily = useQuery({
    queryKey: [...SESSIONS_QUERY_KEYS.all, 'daily', days, tz],
    queryFn: () => api.getSessionsDaily(days, tz),
  });
  const clients = useQuery({
    queryKey: [...SESSIONS_QUERY_KEYS.all, 'clients'],
    queryFn: () => api.getSessionsClients(),
  });

  const invalidate = React.useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: SESSIONS_QUERY_KEYS.all });
  }, [queryClient]);

  const data = summary.data;
  const mix = data?.tokens;
  const dayRows = React.useMemo(() => {
    const rows = daily.data?.days ?? [];
    return rows.slice(-Math.max(days, 1));
  }, [daily.data, days]);
  const maxDay = React.useMemo(() => dayRows.reduce((max, day) => Math.max(max, mixTotal(day)), 0), [dayRows]);

  return (
    <div className="sessions-page">
      <PageHeader
        title={t('sessions.title')}
        subtitle={t('sessions.caption')}
        actions={(
          <>
            {[7, 30, 90].map((range) => (
              <button
                key={range}
                type="button"
                className={`sessions-range${days === range ? ' is-active' : ''}`}
                onClick={() => setDays(range)}
                aria-pressed={days === range}
              >
                {t('sessions.range_days', { days: range })}
              </button>
            ))}
            <RefreshButton isRefreshing={summary.isFetching || daily.isFetching} onRefresh={invalidate} />
          </>
        )}
      />
      {summary.isError && <LoadFailure title={t('sessions.title')} onRetry={() => void summary.refetch()} />}
      {data && (
        <div className="sessions-grid">
          <div className="sessions-tile">
            <div className="sessions-tile-label">{t('sessions.total_tokens')}</div>
            <div className="sessions-tile-value">{fmtTokens(data.total_tokens)}</div>
            {mix && <TokenCompositionBar mix={mix} />}
            {mix && (
              <div className="sessions-tile-caption">
                <span>{t('sessions.mix.input')} {fmtTokens(mix.input)}</span>
                <span>{t('sessions.mix.output')} {fmtTokens(mix.output)}</span>
                <span>{t('sessions.mix.cache_read')} {fmtTokens(mix.cache_read)}</span>
                <span>{t('sessions.mix.reasoning')} {fmtTokens(mix.reasoning)}</span>
              </div>
            )}
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
          {data.today && (
            <div className="sessions-tile sessions-tile-today">
              <div className="sessions-tile-label">{t('sessions.today')}</div>
              <div className="sessions-tile-value">{fmtTokens(mixTotal(data.today), )}</div>
              <div className="sessions-tile-caption">
                <span>{t('sessions.mix.output')} {fmtTokens(data.today.output)}</span>
                <span>{t('sessions.messages')} {data.today.messages}</span>
                {data.today.cost_usd > 0 && <span>{t('sessions.cost')} {formatUsd(data.today.cost_usd)}</span>}
              </div>
            </div>
          )}
        </div>
      )}
      {dayRows.length > 0 && (
        <section className="sessions-section">
          <h2>{t('sessions.daily')}</h2>
          <div className="sessions-daily" data-testid="sessions-daily">
            {dayRows.map((day) => {
              const total = mixTotal(day);
              return (
                <div key={day.date} className="sessions-day" title={`${day.date} · ${fmtTokens(total)}`}>
                  <div className="sessions-day-bar" style={{ height: `${maxDay > 0 ? Math.max(4, (total / maxDay) * 100) : 4}%` }} />
                </div>
              );
            })}
          </div>
          <div className="sessions-tile-caption">{t('sessions.horizon')}</div>
          {data?.biggest_day && (
            <div className="sessions-tile-caption">{t('sessions.biggest_day', { day: data.biggest_day })}</div>
          )}
        </section>
      )}
      {data?.weekday && Object.keys(data.weekday).length > 0 && (
        <section className="sessions-section">
          <h2>{t('sessions.weekday')}</h2>
          <div className="sessions-daily sessions-weekday">
            {WEEKDAY_ORDER.map((en) => {
              const value = data.weekday?.[en] ?? 0;
              const max = Math.max(1, ...Object.values(data.weekday ?? {}));
              return (
                <div key={en} className="sessions-day" title={`${t(`sessions.weekday.${en}`)} · ${fmtTokens(value)}`}>
                  <div className="sessions-day-bar" style={{ height: `${Math.max(4, (value / max) * 100)}%` }} />
                </div>
              );
            })}
          </div>
        </section>
      )}
      {data?.models && data.models.length > 0 && (
        <section className="sessions-section">
          <h2>{t('sessions.models')}</h2>
          <table className="sessions-table">
            <thead>
              <tr>
                <th>{t('sessions.col.model')}</th>
                <th>{t('sessions.col.client')}</th>
                <th>{t('sessions.col.tokens')}</th>
                <th>{t('sessions.col.share')}</th>
              </tr>
            </thead>
            <tbody>
              {data.models.slice(0, 12).map((row) => (
                <tr key={`${row.clients}/${row.model}`}>
                  <td>{row.model}</td>
                  <td>{row.clients}</td>
                  <td>{fmtTokens(row.tokens)}</td>
                  <td>{(row.share * 100).toFixed(1)}%</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}
      {clients.data?.clients && clients.data.clients.length > 0 && (
        <section className="sessions-section">
          <h2>{t('sessions.clients')}</h2>
          <div className="sessions-clients">
            {clients.data.clients.map((client) => (
              <div key={client.client} className="sessions-client" data-testid={`sessions-client-${client.client}`}>
                <div className="sessions-client-name">{client.client}</div>
                <div className="sessions-tile-value is-sub">{fmtTokens(client.tokens)}</div>
                <div className="sessions-tile-caption">
                  <span>{t('sessions.sessions')} {client.sessions}</span>
                  <span>{t('sessions.client.files')} {client.files}</span>
                  <span>{t('sessions.client.entries')} {client.entries}</span>
                  {client.unreadable > 0 && <span>{t('sessions.client.unreadable')} {client.unreadable}</span>}
                  {client.oversized > 0 && <span>{t('sessions.client.oversized')} {client.oversized}</span>}
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
