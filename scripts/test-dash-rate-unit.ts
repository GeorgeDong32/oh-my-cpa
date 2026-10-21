import assert from 'node:assert/strict';
import { DEFAULT_DASH_RATE_UNIT, convertTokenRate, parseDashRateUnit } from '../web/src/types/dashRateUnit';

assert.equal(DEFAULT_DASH_RATE_UNIT, 'tpm');
for (const unit of ['tpm', 'tps']) assert.equal(parseDashRateUnit(unit), unit);
for (const value of [undefined, null, '', 'unknown', true, 0, {}, ['tps'], 'TPM']) {
  assert.equal(parseDashRateUnit(value) ?? DEFAULT_DASH_RATE_UNIT, 'tpm');
}
assert.equal(convertTokenRate(60000, 'tpm'), 60000);
assert.equal(convertTokenRate(60000, 'tps'), 1000);
assert.equal(convertTokenRate(90, 'tps'), 1.5);
assert.equal(convertTokenRate(0, 'tps'), 0);
