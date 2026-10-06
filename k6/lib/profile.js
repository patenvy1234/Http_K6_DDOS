/**
 * profile.js — loads and validates /etc/ddos/profile.json at k6 init time.
 *
 * profile.json schema:
 * {
 *   "run_id":            "ab12cd34",
 *   "run_name":          "smoke-test",
 *   "start_at":          "2026-05-24T22:40:00Z",   // ISO-8601, workers sleep until this
 *   "target_rps":        55000,                     // per-VM RPS target
 *   "ramp_up_seconds":   60,
 *   "duration_seconds":  300,
 *   "ramp_down_seconds": 60,
 *   "pattern":           "keepalive-burst",         // see PATTERNS below
 *   "endpoints": [
 *     {
 *       "id":      "light-get",
 *       "url":     "https://target.example.com/api/ping",
 *       "method":  "GET",
 *       "headers": {},
 *       "body":    "",
 *       "weight":  70
 *     },
 *     {
 *       "id":      "heavy-post",
 *       "url":     "https://target.example.com/api/data",
 *       "method":  "POST",
 *       "headers": {"Content-Type": "application/json"},
 *       "body_bytes": 4096,
 *       "weight":  30,
 *       "resolve_to_ip": "1.2.3.4"   // optional: pin TCP connect to this IP (Host/SNI unchanged)
 *     }
 *   ]
 * }
 *
 * pattern values:
 *   keepalive-burst   – max connection reuse, sustained per-VU throughput
 *   new-conn-flood    – new TCP connection per request, noConnectionReuse=true
 */

// open() is available in the k6 init context (before default/setup are called).
const _profilePath = '/etc/ddos/profile.json';
let _raw;
try {
  _raw = open(_profilePath);
} catch (e) {
  throw new Error('profile.js: cannot open ' + _profilePath + ': ' + e);
}

export const profile = JSON.parse(_raw);

/** PATTERNS — valid values for profile.pattern */
export const PATTERNS = {
  KEEPALIVE_BURST: 'keepalive-burst',
  NEW_CONN_FLOOD:  'new-conn-flood',
};

// Validate the minimum required fields so failures surface before the run starts.
(function validateProfile(p) {
  const required = ['run_id', 'run_name', 'target_rps', 'ramp_up_seconds',
    'duration_seconds', 'ramp_down_seconds', 'pattern', 'endpoints'];
  for (const k of required) {
    if (p[k] === undefined || p[k] === null) {
      throw new Error('profile.js: missing required field: ' + k);
    }
  }
  if (!p.endpoints || p.endpoints.length === 0) {
    throw new Error('profile.js: endpoints array must not be empty');
  }
  const valid = Object.values(PATTERNS);
  if (valid.indexOf(p.pattern) === -1) {
    throw new Error('profile.js: unknown pattern "' + p.pattern + '"; expected one of: ' + valid.join(', '));
  }
  for (const ep of p.endpoints) {
    if (!ep.id || !ep.url || !ep.method) {
      throw new Error('profile.js: each endpoint needs id, url, method; got: ' + JSON.stringify(ep));
    }
    if (ep.body_bytes !== undefined && (!Number.isInteger(ep.body_bytes) || ep.body_bytes < 0)) {
      throw new Error('profile.js: body_bytes must be a non-negative integer for endpoint "' + ep.id + '"');
    }
  }
}(profile));

/**
 * buildWeightedPicker returns a function that picks an endpoint at random
 * proportional to each endpoint's weight (integers 1-100, needn't sum to 100).
 */
export function buildWeightedPicker(endpoints) {
  if (endpoints.length === 0) {
    throw new Error('profile.js: endpoint picker requires at least one endpoint');
  }
  let maxWeight = 0;
  for (const endpoint of endpoints) {
    const weight = endpoint.weight === undefined ? 1 : endpoint.weight;
    if (!Number.isFinite(weight) || weight <= 0) {
      throw new Error('profile.js: endpoint weights must be finite and positive');
    }
    maxWeight = Math.max(maxWeight, weight);
  }
  if (endpoints.length === 1) {
    return () => endpoints[0];
  }
  const cumulative = [];
  let total = 0;
  for (const ep of endpoints) {
    total += (ep.weight === undefined ? 1 : ep.weight) / maxWeight;
    cumulative.push({ ep, threshold: total });
  }
  return function pick() {
    const r = Math.random() * total;
    for (const c of cumulative) {
      if (r < c.threshold) return c.ep;
    }
    return cumulative[cumulative.length - 1].ep;
  };
}

/**
 * totalDurationStr returns a k6-format duration string covering ramp-up +
 * steady-state + ramp-down so the scenario shuts down cleanly.
 */
export function totalDurationStr(p) {
  const s = p.ramp_up_seconds + p.duration_seconds + p.ramp_down_seconds;
  return s + 's';
}

/**
 * rampStages converts the profile's ramp fields into k6 stages array usable
 * by a ramping-arrival-rate executor. Not used by constant-arrival-rate (which
 * uses gracefulStop instead) but useful for future ramping scenarios.
 */
export function rampStages(p) {
  return [
    { duration: p.ramp_up_seconds + 's',   target: p.target_rps },
    { duration: p.duration_seconds + 's',  target: p.target_rps },
    { duration: p.ramp_down_seconds + 's', target: 0 },
  ];
}
