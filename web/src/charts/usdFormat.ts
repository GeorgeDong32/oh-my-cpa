/**
 * formatUsdAmount renders a chart-axis-scale dollar amount: whole dollars
 * once they are big enough to read that way, thousands and millions
 * abbreviated like the token axis abbreviates counts.
 */
export function formatUsdAmount(value: number): string {
  if (!Number.isFinite(value)) return '—';
  const abs = Math.abs(value);
  if (abs >= 1_000_000) return `$${(value / 1_000_000).toFixed(1)}M`;
  if (abs >= 1_000) return `$${(value / 1_000).toFixed(1)}K`;
  if (abs < 0.005) return '$0';
  // Axis ticks read better without cents once the dollars are two digits.
  if (abs >= 10) return `$${value.toFixed(0)}`;
  return `$${value.toFixed(2)}`;
}
