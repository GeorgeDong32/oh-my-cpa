export type DashRateUnit = 'tpm' | 'tps';

export const DEFAULT_DASH_RATE_UNIT: DashRateUnit = 'tpm';
export const DASH_RATE_UNIT_PREFERENCE = 'omc_dash_rate_unit';

export function parseDashRateUnit(raw: unknown): DashRateUnit | undefined {
  return raw === 'tpm' || raw === 'tps' ? raw : undefined;
}

/** The dashboard's token rate is per minute; per second divides by 60. */
export function convertTokenRate(perMinute: number, unit: DashRateUnit): number {
  return unit === 'tps' ? perMinute / 60 : perMinute;
}
