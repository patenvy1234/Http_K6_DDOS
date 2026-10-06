/**
 * keepalive-burst.js — maximum connection reuse, high-RPS GET/POST flood.
 *
 * Executor: constant-arrival-rate
 *   • k6 dispatches `target_rps` arrivals per second.
 *   • With noConnectionReuse=false (default), each VU reuses its TCP connection
 *     across iterations → low syscall overhead, high throughput.
 *   • preAllocatedVUs = ceil(target_rps / 1500) to start; k6 grows to maxVUs if
 *     VUs become the bottleneck.
 *
 * Best for: connection-pool capacity tests, HTTP/1.1 keep-alive vs HTTP/2,
 *           steady-state RPS ceiling of the target.
 */

import { profile, buildWeightedPicker, totalDurationStr } from '../lib/profile.js';
import { KEEPALIVE_PARAMS, buildParams, buildPayloads, sendRequest } from '../lib/transport.js';
import { record } from '../lib/metrics.js';

// Pre-build all payloads and param objects at init time (outside hot path).
const _payloads = buildPayloads(profile.endpoints, 256);
const _paramsByEp = {};
for (const ep of profile.endpoints) {
  _paramsByEp[ep.id] = buildParams(ep, KEEPALIVE_PARAMS);
}
const _pick = buildWeightedPicker(profile.endpoints);

// VU sizing: 1 VU per 1500 expected RPS (keep-alive is very efficient).
const _preAlloc = Math.ceil(profile.target_rps / 1500) + 10;
const _maxVUs   = Math.min(_preAlloc * 4, 5000);

export const options = {
  scenarios: {
    'keepalive-burst': {
      executor:           'constant-arrival-rate',
      rate:               profile.target_rps,
      timeUnit:           '1s',
      duration:           totalDurationStr(profile),
      preAllocatedVUs:    _preAlloc,
      maxVUs:             _maxVUs,
      gracefulStop:       profile.ramp_down_seconds + 's',
    },
  },
  // Don't fail the test on high error rates — we're measuring, not gating.
  thresholds: {},
  discardResponseBodies: true,
};

export default function () {
  const ep = _pick();
  const res = sendRequest(ep, _paramsByEp[ep.id], _payloads[ep.id]);
  record(res, ep.id, _payloads[ep.id] ? _payloads[ep.id].length : 0);
}
