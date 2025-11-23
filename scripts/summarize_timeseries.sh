#!/usr/bin/env bash
set -euo pipefail

FILE="${1:-}"
if [[ -z "$FILE" || ! -f "$FILE" ]]; then
  echo "Usage: $0 /path/to/timeseries.csv" >&2
  exit 2
fi

echo "== Timeseries quick stats =="
awk -F',' '
NR==1{
  for(i=1;i<=NF;i++){
    name=$i; gsub(/[ \t\r]/, "", name); g[name]=i
  }
  next
}
{
  i_idx=g["inflight"]; if(i_idx){ v=$i_idx+0; if(v>peak_inflight) peak_inflight=v; last_inflight=v }
  g_idx=g["gaps"]; if(g_idx){ v=$g_idx+0; if(v>peak_gaps) peak_gaps=v; last_gaps=v }
  m_idx=g["memory_mib"]; if(m_idx){ v=$m_idx+0; if(v>peak_mem) peak_mem=v }
  l_idx=g["avg_processing_latency_ms"]; if(l_idx){ v=$l_idx+0; sum_lat+=v; cnt++ }
  w_idx=g["active_workers"]; if(w_idx){ v=$w_idx+0; if(min_workers==0||v<min_workers) min_workers=v; if(v>max_workers) max_workers=v }
  o_idx=g["ownership_mismatches"]; if(o_idx){ v=$o_idx+0; if(v>peak_mismatch) peak_mismatch=v; last_mismatch=v }
}
END{
  avg_lat=(cnt?sum_lat/cnt:0)
  printf("peak_inflight=%d\nlast_inflight=%d\npeak_gaps=%d\nlast_gaps=%d\npeak_mem_mib=%.2f\navg_processing_latency_ms=%.2f\nworkers_min=%d\nworkers_max=%d\nlast_ownership_mismatches=%d\npeak_ownership_mismatches=%d\n", peak_inflight, last_inflight, peak_gaps, last_gaps, peak_mem, avg_lat, min_workers, max_workers, last_mismatch, peak_mismatch)
}
' "$FILE"
