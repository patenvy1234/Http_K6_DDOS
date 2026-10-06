/**
 * transport.js — optimized HTTP request helpers.
 *
 * Design principles for 1M RPS-class loads:
 *   1. responseType:'none'   — never buffer or parse response bodies; saves
 *      GC pressure and network buffer allocations in the k6 engine.
 *   2. Pre-built body strings — POST/PUT/PATCH bodies are built once at init
 *      time and reused across all iterations; avoids per-iteration GC.
 *   3. throw:false           — on network errors, return the response object
 *      rather than raising a JS exception; exception handling has real cost at
 *      high iteration rates.
 *   4. Connection reuse      — configured globally in the entrypoint options
 *      according to the selected profile pattern.
 *   5. Minimal headers       — every header is CPU work in Goja; only add what
 *      the target needs.
 */

import http from 'k6/http';

/**
 * BASE_PARAMS — shared params for all scenarios.
 * Override per-scenario with Object.assign({}, BASE_PARAMS, overrides).
 */
export const BASE_PARAMS = {
  responseType: 'none',   // discard response body
  throw: false,           // return Response on error; don't throw
  timeout: '15s',         // generous but bounded
  tags: {},               // caller adds { endpoint: id }
};

/**
 * KEEPALIVE_PARAMS — for maximum connection reuse.
 * Connection reuse is controlled by the entrypoint's global k6 options.
 */
export const KEEPALIVE_PARAMS = Object.assign({}, BASE_PARAMS);

/**
 * NEW_CONN_PARAMS — for new-connection-per-request scenarios.
 * The entrypoint sets noConnectionReuse globally for this pattern.
 */
export const NEW_CONN_PARAMS = Object.assign({}, BASE_PARAMS);

/**
 * buildParams merges endpoint-level headers + an endpoint tag into base params.
 *
 * @param {object} endpoint - profile endpoint object
 * @param {object} base     - one of KEEPALIVE_PARAMS or NEW_CONN_PARAMS
 * @returns {object} merged params safe to pass directly to http.get/post/…
 */
export function buildParams(endpoint, base) {
  const p = Object.assign({}, base);
  if (endpoint.headers && Object.keys(endpoint.headers).length > 0) {
    p.headers = endpoint.headers;
  }
  p.tags = { endpoint: endpoint.id };
  return p;
}

/**
 * buildPayloads pre-allocates body strings for every endpoint at init time.
 * Returns a Map<endpoint.id → string|null>.
 *
 * For POST/PUT/PATCH, a synthetic JSON payload is generated at the endpoint's
 * configured `body_bytes` size, falling back to `defaultBodyBytes`.
 *
 * @param {Array}  endpoints      - from profile.endpoints
 * @param {number} defaultBodyBytes - bytes to generate when no body is set
 */
export function buildPayloads(endpoints, defaultBodyBytes) {
  const payloads = {};
  const WRITE_METHODS = { POST: 1, PUT: 1, PATCH: 1 };
  for (const ep of endpoints) {
    if (WRITE_METHODS[ep.method.toUpperCase()]) {
      // Generate a stable JSON blob of approximately the requested size.
      payloads[ep.id] = syntheticJSON(ep.body_bytes || defaultBodyBytes || 512);
    } else {
      payloads[ep.id] = null;
    }
  }
  return payloads;
}

/**
 * sendRequest dispatches a single HTTP request for an endpoint.
 * Returns the k6 Response object.
 *
 * @param {object} endpoint - profile endpoint
 * @param {object} params   - merged http params (from buildParams)
 * @param {string|null} body - pre-built body string or null
 */
export function sendRequest(endpoint, params, body) {
  return http.request(endpoint.method.toUpperCase(), endpoint.url, body, params);
}

/**
 * syntheticJSON builds a repeating-character JSON string of approximately
 * `targetBytes` in length. Built once at init; cheap to pass around.
 */
export function syntheticJSON(targetBytes) {
  // {"payload":"<data>"}
  const overhead = '{"payload":"","seq":0}'.length;
  const dataLen = Math.max(1, targetBytes - overhead);
  // Use a fixed repeating pattern to allow server-side compression but
  // still push real bytes on the wire for uncompressed targets.
  const chunk = 'abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_-';
  const data = chunk.repeat(Math.ceil(dataLen / chunk.length));
  return '{"payload":"' + data.slice(0, dataLen) + '","seq":0}';
}
