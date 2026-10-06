/**
 * metrics.js — minimal record() shim.
 *
 * For the scaler, we only need k6's NATIVE counters: k6_http_reqs_total
 * and k6_dropped_iterations_total. Every other metric (custom counters,
 * trends, gauges, per-endpoint tags) inflates remote_write payload size
 * and slows DCE ingestion. They are intentionally NOT emitted.
 *
 * If you need per-endpoint debugging later, re-enable them here — but
 * be aware that high-cardinality tags will throttle the AMW pipeline
 * once the fleet exceeds a handful of VMs.
 */

export function record(_res, _epId, _bodyLen) {
  // intentionally empty — native k6 metrics suffice
}

