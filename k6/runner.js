/**
 * runner.js — k6 entry point deployed to /etc/ddos/runner.js on each worker VM.
 *
 * Reads /etc/ddos/profile.json at init time and:
 *   1. Builds the scenario configuration (executor, rate, duration, VU sizing)
 *      from profile fields.
 *   2. Dispatches all requests through the appropriate transport helper
 *      (keepalive vs new-conn) and records custom metrics.
 *
 * The ddos-worker.service systemd unit invokes k6 as:
 *   k6 run --quiet --compatibility-mode=base
 *          --tag runId=... --tag region=... --tag instance=...
 *          /etc/ddos/runner.js
 *
 * K6_OUT=experimental-prometheus-rw and K6_PROMETHEUS_RW_SERVER_URL are set
 * via /etc/ddos/runtime.env; K6_PROMETHEUS_RW_HEADERS (Bearer token) via
 * /etc/ddos/k6.env, refreshed every 50 min by ddos-token-refresh.service.
 *
 * --compatibility-mode=base means Goja ES6+ without Babel; import/export work.
 * Do NOT use require() or Node.js built-ins.
 */

import { profile, PATTERNS, buildWeightedPicker, totalDurationStr } from './lib/profile.js';
import {
  KEEPALIVE_PARAMS, NEW_CONN_PARAMS,
  buildParams, buildPayloads, sendRequest,
} from './lib/transport.js';
import { record } from './lib/metrics.js';

// ---------------------------------------------------------------------------
// Scenario & VU sizing (derived once at init)
// ---------------------------------------------------------------------------

const _pattern  = profile.pattern;
const _rps      = profile.target_rps;
const _durStr   = totalDurationStr(profile);
const _graceStr = profile.ramp_down_seconds + 's';

/**
 * Return per-scenario VU pre-allocation based on the traffic pattern.
 *
 * Sizing rationale (assumes ~25-50 ms RTT to public targets — Azure Front
 * Door, CDN, public API):
 *   - keepalive-burst: each VU can sustain ~30-50 iter/s after TCP+TLS warm-up
 *     (limited by RTT, not CPU). Pre-allocating rps/40 + 50 covers steady
 *     state without thrashing the VU pool. maxVUs grows to 8x pre-allocation.
 *   - new-conn-flood: each VU stalls on every TCP+TLS handshake (~80-150 ms),
 *     so we need rps/30 + 200 VUs and up to 3x pre-allocation.
 *
 * If preAllocatedVUs is too low, k6's executor grows the pool lazily; while
 * that happens iterations queue up and surface as dropped_iterations cliffs
 * (which the scaler then misreads as a "deficit", causing false scale-outs).
 * Bounding growth avoids an 8 GB worker trying to allocate tens of thousands
 * of VUs when the target is slow. Dropped iterations then tell the controller
 * to scale horizontally instead of letting one worker run out of memory.
 *
 * NOTE: per-VM target_rps is capped at 5,000 by the Go controller
 * (profilegen.FixedPerVMRPS). VU budgets sized for that ceiling.
 */
function vuBudget(rps, pattern) {
  switch (pattern) {
    case PATTERNS.NEW_CONN_FLOOD: {
      const pre = Math.ceil(rps / 30) + 200;
      return { pre, max: Math.min(pre * 3, 50000) };
    }
    default: { /* keepalive-burst */
      const pre = Math.ceil(rps / 40) + 50;
      return { pre, max: Math.min(pre * 8, 5000) };
    }
  }
}

const _vu = vuBudget(_rps, _pattern);

// ---------------------------------------------------------------------------
// HTTP params — built once, reused across all iterations
// ---------------------------------------------------------------------------

const _baseParam = (_pattern === PATTERNS.NEW_CONN_FLOOD)
  ? NEW_CONN_PARAMS
  : KEEPALIVE_PARAMS;

const _payloads   = buildPayloads(profile.endpoints, 512);
const _paramsByEp = {};
for (const ep of profile.endpoints) {
  _paramsByEp[ep.id] = buildParams(ep, _baseParam);
}

// Weighted endpoint picker.
const _pick = buildWeightedPicker(profile.endpoints);

// ---------------------------------------------------------------------------
// hosts map — pin hostnames to specific IPs where requested
// ---------------------------------------------------------------------------

// Build K6 hosts map from endpoints that carry resolve_to_ip.
// K6's options.hosts resolves the hostname to the given IP while keeping the
// original Host header and TLS SNI intact, so the server sees the correct
// virtual host even though the TCP connection goes to a different address.
const _hosts = {};
for (const ep of profile.endpoints) {
  if (ep.resolve_to_ip) {
    // Extract bare hostname from URL (strip scheme, port, path).
    const m = ep.url.match(/^https?:\/\/([^/:?#]+)/);
    if (m) {
      _hosts[m[1]] = ep.resolve_to_ip;
    }
  }
}

// ---------------------------------------------------------------------------
// k6 options — single source of truth
// ---------------------------------------------------------------------------

export const options = {
  scenarios: {
    [_pattern]: {
      executor: 'constant-arrival-rate',
      rate: _rps, timeUnit: '1s',
      duration: _durStr,
      preAllocatedVUs: _vu.pre,
      maxVUs: _vu.max,
      gracefulStop: _graceStr,
    },
  },
  thresholds:             {},
  discardResponseBodies:  true,
  // Keep only the scenario system tag. CLI --tag values (runId, runName, region,
  // instance) are user tags and are always included regardless of this setting.
  // Stripping status/method/url/error etc. reduces remote_write series count,
  // which lowers DCE ingestion latency and avoids k6 sample-drop warnings.
  systemTags: ['scenario'],
  noConnectionReuse:      _pattern === PATTERNS.NEW_CONN_FLOOD,
  // Resolve hostnames to specific IPs (populated from endpoints with resolve_to_ip).
  // Host header and TLS SNI stay unchanged — only the TCP connect target is overridden.
  ...(Object.keys(_hosts).length > 0 ? { hosts: _hosts } : {}),
};

// ---------------------------------------------------------------------------
// Default function — hot path
// ---------------------------------------------------------------------------

export default function () {
  const ep  = _pick();
  const res = sendRequest(ep, _paramsByEp[ep.id], _payloads[ep.id]);
  record(res, ep.id, _payloads[ep.id] ? _payloads[ep.id].length : 0);
}
