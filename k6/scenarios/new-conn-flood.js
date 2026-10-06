/**
 * new-conn-flood.js — new TCP connection per request.
 *
 * Executor: constant-arrival-rate
 *   • noConnectionReuse=true forces a fresh TCP handshake for every request,
 *     maximising half-open sockets and exercising the server's accept queue,
 *     SYN backlog, and connection tracking tables.
 *   • Because each VU needs its own socket-setup time, throughput per VU is
 *     ~10-20× lower than keepalive. We need more VUs for the same arrival rate.
 *   • preAllocatedVUs = ceil(target_rps / 100).
 *
 * Best for: testing SYN/accept queue limits, connection-state table exhaustion,
 *           TCP stack resilience under new-connection pressure.
 *
 * Egress: each VM has its OWN Standard SKU public IPv4 (no NAT Gateway).
 * That gives the VM the full ~64k ephemeral port range against any single
 * 5-tuple destination, so SNAT port exhaustion is a non-issue at our scale.
 * Kernel-side tuning (tcp_tw_reuse + reduced tcp_fin_timeout in
 * sysctl/99-ddos.conf) keeps the recycle rate high enough for sustained
 * new-conn floods.
 */

import { profile, buildWeightedPicker, totalDurationStr } from '../lib/profile.js';
import { NEW_CONN_PARAMS, buildParams, buildPayloads, sendRequest } from '../lib/transport.js';
import { record } from '../lib/metrics.js';

const _payloads   = buildPayloads(profile.endpoints, 256);
const _paramsByEp = {};
for (const ep of profile.endpoints) {
  _paramsByEp[ep.id] = buildParams(ep, NEW_CONN_PARAMS);
}
const _pick = buildWeightedPicker(profile.endpoints);

// VU sizing: 1 VU per 100 RPS (new-conn is expensive; each VU blocks on TCP
// setup and teardown). Add slack to avoid arrival-rate drops on busy hosts.
const _preAlloc = Math.ceil(profile.target_rps / 100) + 50;
const _maxVUs   = Math.min(_preAlloc * 3, 50000);

export const options = {
  scenarios: {
    'new-conn-flood': {
      executor:           'constant-arrival-rate',
      rate:               profile.target_rps,
      timeUnit:           '1s',
      duration:           totalDurationStr(profile),
      preAllocatedVUs:    _preAlloc,
      maxVUs:             _maxVUs,
      gracefulStop:       profile.ramp_down_seconds + 's',
    },
  },
  thresholds: {},
  discardResponseBodies: true,
  noConnectionReuse: true,
};

export default function () {
  const ep = _pick();
  const res = sendRequest(ep, _paramsByEp[ep.id], _payloads[ep.id]);
  record(res, ep.id, _payloads[ep.id] ? _payloads[ep.id].length : 0);
}
