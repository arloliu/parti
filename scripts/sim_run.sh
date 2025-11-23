#!/usr/bin/env bash
set -euo pipefail

# sim_run.sh
# Run the Parti simulation for a specified duration using a base config,
# capture logs, and export metrics to CSV (timeseries + summary).
#
# Usage:
#   scripts/sim_run.sh -config test/simulation/configs/stress-short.yaml -duration 10m -label v2-chaos -outdir /tmp/simruns
#   scripts/sim_run.sh -config test/simulation/configs/stress-short-no-chaos.yaml -duration 30m
#
# Requirements:
# - go toolchain available
# - repo root is current working directory (or set REPO_ROOT)
# - Optional: yq (if present, will be used to set duration precisely). Falls back to sed.

REPO_ROOT=${REPO_ROOT:-$(pwd)}
CONFIG=""
DURATION=""
OUTDIR=""
LABEL=""
SCALE_UP_ONCE=""
COOLDOWN=""

# --- Functions ---

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -config)
        CONFIG="$2"; shift 2 ;;
      -duration)
        DURATION="$2"; shift 2 ;;
      -outdir)
        OUTDIR="$2"; shift 2 ;;
      -label)
        LABEL="$2"; shift 2 ;;
      -scale-up-once)
        SCALE_UP_ONCE="$2"; shift 2 ;;
      -cooldown)
        COOLDOWN="$2"; shift 2 ;;
      -h|--help)
        echo "Usage: $0 -config <path> -duration <10m|30m|1h> [-outdir <dir>] [-label <tag>] [-scale-up-once <N>] [-cooldown <30s|1m>]"; exit 0 ;;
      *) echo "Unknown arg: $1"; exit 2 ;;
    esac
  done

  if [[ -z "$CONFIG" ]]; then
    echo "Missing -config" >&2
    exit 2
  fi
  if [[ ! -f "$CONFIG" ]]; then
    echo "Config not found: $CONFIG" >&2
    exit 2
  fi
}

setup_env() {
  TS=$(date +%Y%m%d-%H%M%S)
  BASE_OUTDIR=${OUTDIR:-/tmp/parti-sim}
  RUN_DIR="$BASE_OUTDIR/$TS${LABEL:+-$LABEL}"
  mkdir -p "$RUN_DIR"
  LOG="$RUN_DIR/run.log"
  TIMES_CSV="$RUN_DIR/timeseries.csv"
  SUMMARY_CSV="$RUN_DIR/summary.csv"
}

prepare_config() {
  TMP_CFG="$RUN_DIR/config.yaml"
  cp "$CONFIG" "$TMP_CFG"

  if [[ -n "$DURATION" ]]; then
    if command -v yq >/dev/null 2>&1; then
      DURATION_ENV="$DURATION" yq -i '(.simulation.duration) = env(DURATION_ENV)' "$TMP_CFG"
    else
      # Fallback: replace the first occurrence of "duration: <...>" under simulation block.
      # Simple heuristic: replace the first line that starts with optional spaces then 'duration:'
      # Assumes only one such key exists (our configs keep duration under simulation:).
      sed -E -i "0,/^[[:space:]]*duration:[[:space:]].*$/s//  duration: $DURATION/" "$TMP_CFG"
    fi
  fi
}

run_simulation() {
  local dur_msg="${DURATION:-default}"
  echo "Running simulation: config=$CONFIG duration=$dur_msg → $RUN_DIR"
  echo "Log: $LOG"

  # Run the simulation (foreground). Use tee to capture logs.
  (
    cd "$REPO_ROOT"
    CMD=(go run ./test/simulation/cmd/simulation -config "$TMP_CFG")
    if [[ -n "$SCALE_UP_ONCE" && "$SCALE_UP_ONCE" != "0" ]]; then
      CMD+=( -scale-up-once "$SCALE_UP_ONCE" )
    fi
    if [[ -n "$COOLDOWN" ]]; then
      CMD+=( -cooldown "$COOLDOWN" )
    fi
    echo "Running: ${CMD[*]}"
    "${CMD[@]}" 2>&1 | tee "$LOG"
  )
}

generate_timeseries() {
  awk '
  BEGIN {
    FS="[[:space:]]+"; OFS=",";
    print "timestamp,total_sent,total_received,received_events,inflight,pending_holes,gaps,duplicates,active_workers,active_goroutines,memory_mib,avg_partitions_per_worker,avg_rebalance_duration_s,avg_processing_latency_ms,unassigned,locality_ratio,stable_locality_ratio,moved_partitions_total,cold_start_complete,ownership_mismatches,active_assigned_align,disorder_depth,holes_healed,healed_rate_per_sec,pubcon_samples,pubcon_sum_s,recovery_samples,recovery_sum_s"
  }
  function emit(){
    if (ts=="") return;
    print ts,tsent,trecv,trecv_ev,iflt,pholes,gaps,dups,aw,ag,mem,apw,ard,apl,unass,loc,sloc,moved,cold,ownm,align,dis,healed,hrate,psamp,psum,rsamp,rsum
  }
  /^=== Simulation Report \[/ {
    emit();
    tsent=trecv=trecv_ev=iflt=pholes=gaps=dups=aw=ag=mem=apw=ard=apl=unass=loc=sloc=moved=cold=ownm=align=dis=healed=hrate=psamp=psum=rsamp=rsum="";
    ts = $0;
    sub(/^=== Simulation Report \[/, "", ts); sub(/\] ===$/, "", ts);
  }
  /^Total Sent:/ { tsent=$NF }
  /^Total Received:/ { trecv=$NF }
  /^Received Events:/ { trecv_ev=$NF }
  /^In-Flight:/ { iflt=$NF }
  /^Pending Holes:/ { pholes=$NF }
  /^Gaps Detected:/ { gaps=$NF }
  /^Duplicates:/ { dups=$NF }
  /^Active Workers:/ { aw=$NF }
  /^Active Goroutines:/ { line=$0; gsub(/,/, "", line); split(line, a, /[ ]+/); ag=a[length(a)] }
  /^Memory Usage:/ { mem_line=$0; gsub(/,/, "", mem_line); split(mem_line, a, /[ ]+/); mem=a[length(a)-1] }
  /^Avg Partitions\/Worker:/ { apw=$NF }
  /^Avg Rebalance Duration:/ { ard=$NF; sub(/s$/, "", ard) }
  /^Avg Processing Latency:/ { apl=$NF; sub(/ms$/, "", apl) }
  /^Unassigned Partitions:/ { unass=$NF }
  /^Locality Ratio:/ { loc=$NF }
  /^Stable Locality Ratio:/ { sloc=$NF }
  /^Moved Partitions Total:/ { moved=$NF }
  /^Cold Start Complete:/ { cold=$NF }
  /^Ownership Mismatches:/ { split($0, a, /[ ]+/); for(i=1;i<=length(a);i++){ if (a[i] ~ /^[0-9]+$/) { ownm=a[i]; break } } }
  /^Active-Assigned Align:/ { align=$NF }
  /^Disorder Depth:/ { dis=$NF }
  /^Holes Healed:/ { healed=$NF }
  /^Healed Rate \(last 5s\):/ { line=$0; sub(/^.*: /, "", line); sub(/\/sec$/, "", line); hrate=line }
  /^Publish/ { c=$0; gsub(/,/, "", c); match(c, /Samples: ([0-9]+)/, m1); if(m1[1] != "") psamp=m1[1]; match(c, /sum=([0-9.]+)s/, m2); if(m2[1] != "") psum=m2[1] }
  /^Recovery Samples:/ { c=$0; match(c, /Samples: ([0-9]+)/, m1); if(m1[1] != "") rsamp=m1[1]; match(c, /sum=([0-9.]+)s/, m2); if(m2[1] != "") rsum=m2[1] }
  END { emit() }
  ' "$LOG" > "$TIMES_CSV"
}

generate_summary() {
  awk -F, '
  BEGIN {
    print "metric,min,max,last"
    # Initialize min values to something large
    min_inflight=999999999; min_goroutines=999999999; min_mem=999999999;
    min_loc=999999999; min_sloc=999999999; min_gaps=999999999;
    min_disorder=999999999; min_pholes=999999999;
  }
  NR > 1 {
    # Columns:
    # 5: inflight, 6: pending_holes, 7: gaps, 10: goroutines, 11: memory,
    # 16: locality, 17: stable_locality, 18: moved, 20: mismatches,
    # 22: disorder, 23: healed, 24: healed_rate

    # Inflight (5)
    v=$5; if(v<min_inflight) min_inflight=v; if(v>max_inflight) max_inflight=v; last_inflight=v;
    # Goroutines (10) - strip commas if any (though CSV should be clean)
    v=$10; gsub(/,/, "", v); if(v<min_goroutines) min_goroutines=v; if(v>max_goroutines) max_goroutines=v; last_goroutines=v;
    # Memory (11)
    v=$11; if(v<min_mem) min_mem=v; if(v>max_mem) max_mem=v; last_mem=v;
    # Locality (16)
    v=$16; if(v<min_loc) min_loc=v; if(v>max_loc) max_loc=v; last_loc=v;
    # Stable Locality (17)
    v=$17; if(v<min_sloc) min_sloc=v; if(v>max_sloc) max_sloc=v; last_sloc=v;
    # Gaps (7)
    v=$7; if(v<min_gaps) min_gaps=v; if(v>max_gaps) max_gaps=v; last_gaps=v;
    # Disorder (22)
    v=$22; if(v<min_disorder) min_disorder=v; if(v>max_disorder) max_disorder=v; last_disorder=v;
    # Pending Holes (6)
    v=$6; if(v<min_pholes) min_pholes=v; if(v>max_pholes) max_pholes=v; last_pholes=v;

    # Last-only metrics
    last_moved=$18;
    last_mismatches=$20;
    last_healed=$23;
    last_rate=$24;
  }
  END {
    printf "inflight,%s,%s,%s\n", min_inflight, max_inflight, last_inflight
    printf "goroutines,%s,%s,%s\n", min_goroutines, max_goroutines, last_goroutines
    printf "memory_mib,%s,%s,%s\n", min_mem, max_mem, last_mem
    printf "locality_ratio,%s,%s,%s\n", min_loc, max_loc, last_loc
    printf "stable_locality_ratio,%s,%s,%s\n", min_sloc, max_sloc, last_sloc
    printf "gaps,%s,%s,%s\n", min_gaps, max_gaps, last_gaps
    printf "disorder_depth,%s,%s,%s\n", min_disorder, max_disorder, last_disorder
    printf "pending_holes,%s,%s,%s\n", min_pholes, max_pholes, last_pholes
    printf "moved_partitions_total,,,%s\n", last_moved
    printf "ownership_mismatches,,,%s\n", last_mismatches
    printf "holes_healed_total,,,%s\n", last_healed
    printf "healed_rate_per_sec,,,%s\n", last_rate
  }
  ' "$TIMES_CSV" > "$SUMMARY_CSV"
}

generate_gap_timeline() {
  GAP_TL_TXT="$RUN_DIR/gap_timeline.txt"
  GAP_TL_JSON="$RUN_DIR/gap_timeline.json"
  (
    cd "$REPO_ROOT"
    if go run ./scripts/gap_timeline -dir "$RUN_DIR" > "$GAP_TL_TXT"; then
      :
    else
      echo "Warning: failed to generate gap timeline text output" >&2
    fi
    if go run ./scripts/gap_timeline -dir "$RUN_DIR" -json > "$GAP_TL_JSON"; then
      :
    else
      echo "Warning: failed to generate gap timeline JSON output" >&2
    fi
  )

  if [[ -f "$GAP_TL_TXT" ]]; then
    echo "  Gap Timeline:       $GAP_TL_TXT"
    # Check for critical errors in the timeline
    if grep -q "!!!" "$GAP_TL_TXT"; then
      echo -e "\n\033[0;31mWARNING: Critical events detected in timeline:\033[0m"
      grep "!!!" "$GAP_TL_TXT" | head -n 5
      if [[ $(grep -c "!!!" "$GAP_TL_TXT") -gt 5 ]]; then
        echo "... (see $GAP_TL_TXT for full list)"
      fi
    fi
    # Print summary from the end of the file
    if grep -q "SUMMARY:" "$GAP_TL_TXT"; then
       echo -e "\nTimeline Summary:"
       sed -n '/SUMMARY:/,$p' "$GAP_TL_TXT" | tail -n +2 | sed 's/^/  /'
    fi
  fi
  if [[ -f "$GAP_TL_JSON" ]]; then
    echo "  Gap Timeline (JSON): $GAP_TL_JSON"
  fi
}

# --- Main ---

parse_args "$@"
setup_env
prepare_config
run_simulation
generate_timeseries
generate_summary
generate_gap_timeline

echo
echo "Outputs:"
echo "  Log:        $LOG"
echo "  Timeseries: $TIMES_CSV"
echo "  Summary:    $SUMMARY_CSV"
echo "  Config:     $TMP_CFG"
echo "Done."
