import React from 'react';
import { Column } from '@ant-design/charts';
import { ChartMount } from './ChartMount';
import dayjs from '../utils/time';
import { renderChartTooltip } from './chartTooltip';
import { useTheme } from '../theme/ThemeContext';
import { usePrefersReducedMotion } from '../hooks/usePrefersReducedMotion';
import { resolveChartAnimation } from './chartMotion';
import { useChartPlugins } from './chartRender';
import { seriesColor } from './chartTheme';
import { currentFontStack } from '../theme/fontLibrary';
import { formatTokens as formatTokensStyled, formatTokensFull } from '../types/tokenDisplay';
import { useTokenDisplayStyle } from '../types/tokenDisplayContext';
import type { SessionsSeriesPoint } from '../types/sessions';

export interface SessionsTrendChartProps {
  /** One flat row per date per series; the total mode carries the single `total` series. */
  points: SessionsSeriesPoint[];
  height?: number;
}

/** The remainder bucket's key: several minor series folded into one grey stack. */
export const SESSIONS_OTHER_SERIES = '__other__';

/**
 * SessionsTrendChart draws the sessions page's daily usage as one stacked
 * column per calendar day, in the viewer's own timezone.
 *
 * **The mark is a column, not an area.** Unlike the dashboard's zero-filled
 * bucket grids, the sessions window is day-grained and sparse at its start: a
 * range the operator just widened has days with no rows at all, and an area
 * would draw those as a continuous band that reads as data. Columns leave
 * genuine gaps as gaps.
 *
 * **Colour follows rank, once.** Each series' hue resolves from the theme
 * palette by its rank in `seriesNames` — the same agreement ModelTokenTrend
 * buys with seriesDomainKey — so a series' column and its tooltip row cannot
 * wear different hues, and the remainder stack is the one grey.
 */
export const SessionsTrendChart: React.FC<SessionsTrendChartProps> = ({ points, height = 280 }) => {
  const { theme } = useTheme();
  const isReducedMotion = usePrefersReducedMotion();
  const animate = resolveChartAnimation(isReducedMotion);
  const { style: tokenStyle } = useTokenDisplayStyle();
  const { plugins, onReady } = useChartPlugins();

  const seriesNames = React.useMemo(() => {
    const names: string[] = [];
    for (const point of points) {
      if (!names.includes(point.series)) names.push(point.series);
    }
    return names;
  }, [points]);

  const colorOf = React.useCallback(
    (name: string) =>
      name === SESSIONS_OTHER_SERIES ? theme.palette.muted : seriesColor(theme.palette, seriesNames.indexOf(name)),
    [seriesNames, theme.palette],
  );

  const labelFormat = React.useMemo(() => {
    if (points.length < 2) return 'MM-DD';
    const span = dayjs(points[points.length - 1]?.date).diff(dayjs(points[0]?.date), 'day');
    return span > 92 ? 'YY-MM' : 'MM-DD';
  }, [points]);

  const data = React.useMemo(() => points.map((point) => ({ ...point, value: point.tokens })), [points]);

  return (
    <ChartMount>
      <Column
        data={data}
        xField="date"
        yField="value"
        colorField="series"
        stack
        height={height}
        animation={animate}
        plugins={plugins}
        onReady={onReady}
        scale={{
          color: { range: seriesNames.map(colorOf) },
          // A single-day window paints one band; padding keeps the column a
          // column instead of a full-width slab.
          x: { paddingInner: 0.55, paddingOuter: 0.18 },
        }}
        interaction={{
          // The tooltip is configured on the *interaction*, not on the mark:
          // G2 reads `render` from the interaction's options — a renderer in
          // the mark's own `tooltip` spec is ignored and the library's default
          // template prints bare series names instead of token counts (the
          // exact bug this comment prevents from coming back).
          tooltip: {
            shared: true,
            crosshairs: true,
            render: (
              _event: unknown,
              context: { items?: Array<{ color?: string; value?: number | null; name?: string }>; title?: string },
            ) => {
              const byKey = new Map((context?.items ?? []).map((item) => [String(item.name ?? ''), item]));
              const items = seriesNames
                .slice()
                .reverse()
                .map((name) => {
                  const item = byKey.get(name);
                  const value = Number(item?.value ?? 0);
                  return { name, color: item?.color ?? colorOf(name), value };
                })
                .filter((item) => item.value > 0);
              if (items.length === 0) return '';
              // The bucket arrives as the group's title — the date string itself,
              // since the x field is a calendar day rather than an epoch.
              return renderChartTooltip(
                context?.title ?? '',
                items.map((item) => ({
                  name: item.name,
                  color: item.color,
                  value: formatTokensStyled(item.value, tokenStyle),
                  exact: formatTokensFull(item.value),
                })),
              );
            },
          },
        }}
        axis={{
          x: {
            labelFormatter: (value: string) => dayjs(value).format(labelFormat),
            labelAutoRotate: false,
            labelFontFamily: currentFontStack('mono'),
            labelFontSize: 10,
          },
          y: {
            labelFormatter: (value: number) => formatTokensStyled(value, tokenStyle),
            labelFontFamily: currentFontStack('mono'),
            labelFontSize: 10,
          },
        }}
      />
    </ChartMount>
  );
};
