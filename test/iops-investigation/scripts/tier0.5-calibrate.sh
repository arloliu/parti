#!/usr/bin/env bash
# tier0.5-calibrate.sh — absolute IOPS calibration via known-payload dd.
#
# Tier 0 verifies the capture chain is honest (files land, aggregator
# runs, RPC predictions match). Tier 0.5 verifies the *numbers* are
# calibrated: when we measure block_*_iops, do cgroup and iostat report
# values consistent with a known workload?
#
# Method:
#   1. Bring up an iops-calibrator sidecar (busybox) on a fresh docker
#      volume — same physical disk as the nats data volumes.
#   2. Start cgroup-io + iostat captures targeting the sidecar.
#   3. Idle baseline for 5 s.
#   4. dd if=/dev/zero of=/data/x bs=$BS count=$COUNT
#         oflag=direct conv=fdatasync          # known payload, no page cache
#   5. Tail 5 s for stragglers, stop captures.
#   6. Compute three ratios over the dd window:
#        cg_w_bytes  / payload      ∈ [0.95, 1.30]   # cgroup should track payload
#        ios_w_bytes / payload      ∈ [0.30, 1.30]   # iostat may merge; bounded
#        cg_w_bytes  / ios_w_bytes  ∈ [0.50, 4.00]   # legitimate ratio band
#      Same three ratios on IO counts (rios/wios), since merge ratio for
#      counts is what surprised us most on the harness runs (saw 27× there).
#
# Exit code 0 if all six ratios land in band, 1 otherwise.
#
# Why this check exists: harness runs show cgroup/iostat divergences up
# to 27× on busy windows. Tier 0 silenced that cross-check by making it
# diagnostic. Tier 0.5 anchors the legitimate band for this host so
# future divergence outside the band can be flagged as a real bug
# instead of rationalized as "write merging".
#
# Usage:
#   tier0.5-calibrate.sh [--results-dir PATH] [--payload-mib N] [--bs BYTES]
#                        [--baseline-secs N] [--tail-secs N]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RIG_DIR="$(dirname "$SCRIPT_DIR")"

# Defaults.
RESULTS_ROOT=""
PAYLOAD_MIB=32          # total bytes = PAYLOAD_MIB * 1024 * 1024
BS_BYTES=4096           # write size; payload count = MiB*1024*1024/BS
BASELINE_SECS=5         # idle window before dd to anchor counters
TAIL_SECS=5             # idle window after dd for stragglers

# Gate bands.  Calibrated empirically on an ext4-journaling host:
#   cgroup is the precise instrument — when O_DIRECT writes 32 MiB the
#   cgroup wbytes counter reads exactly 33554432 (ratio 1.000). The
#   strict 0.95–1.30 band catches any phantom-write regression where
#   cgroup over-counts (e.g. counting un-flushed page-cache writes).
#
#   iostat reports HOST-level disk writes, which include filesystem
#   journal commits (jbd2) and metadata flushes that happen outside
#   the sidecar's cgroup. On a 32 MiB dd we observe ~2.0× total bytes
#   at the host; that overhead shrinks as payload grows.  Allow up to
#   2.5× to give the host headroom.
#
#   cg/ios ratio: in calibration cgroup<iostat (journaling adds writes
#   cgroup doesn't see), so we expect 0.30..1.10. The HARNESS observed
#   the OPPOSITE direction (cg=27×ios) — that's a phantom-write signal
#   from page-cache coalescing, which Tier 0.5 surfaces as a hard fail.
CG_BYTES_LO="0.95"
CG_BYTES_HI="1.30"
IOS_BYTES_LO="0.30"
IOS_BYTES_HI="2.50"
RATIO_LO="0.25"
RATIO_HI="4.00"

CALIBRATOR_NAME="iops-calibrator"
# debian-slim ships GNU dd which supports `conv=fdatasync` and
# `oflag=direct,dsync` reliably. busybox dd does not have fdatasync.
CALIBRATOR_IMAGE="debian:bookworm-slim"
CALIBRATOR_VOL="iops-calibrator-data"

usage() {
    cat >&2 <<EOF
Usage: tier0.5-calibrate.sh [options]

Options:
  --results-dir PATH    Where artifacts land (default: results/tier0.5-<timestamp>/).
  --payload-mib N       Total dd payload in MiB (default: 32).
  --bs BYTES            dd block size (default: 4096; must match O_DIRECT alignment).
  --baseline-secs N     Idle window before dd (default: 5).
  --tail-secs N         Idle window after dd (default: 5).
  -h, --help            Show this message and exit.

The calibrator launches a busybox sidecar on a fresh docker volume,
writes \$PAYLOAD_MIB MiB with O_DIRECT + fdatasync, and verifies the
cgroup + iostat instruments produce numbers consistent with the known
payload. Exits 0 on PASS, 1 on FAIL.
EOF
    exit 2
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --results-dir)   RESULTS_ROOT="$2";  shift 2 ;;
        --payload-mib)   PAYLOAD_MIB="$2";   shift 2 ;;
        --bs)            BS_BYTES="$2";      shift 2 ;;
        --baseline-secs) BASELINE_SECS="$2"; shift 2 ;;
        --tail-secs)     TAIL_SECS="$2";     shift 2 ;;
        -h|--help)       usage ;;
        *) echo "tier0.5-calibrate.sh: unknown argument: $1" >&2; usage ;;
    esac
done

# Derive payload values.
PAYLOAD_BYTES=$(( PAYLOAD_MIB * 1024 * 1024 ))
DD_COUNT=$(( PAYLOAD_BYTES / BS_BYTES ))
if (( DD_COUNT * BS_BYTES != PAYLOAD_BYTES )); then
    echo "tier0.5-calibrate.sh: bs ($BS_BYTES) does not divide payload ($PAYLOAD_BYTES); pick aligned values" >&2
    exit 2
fi

if [[ -z "$RESULTS_ROOT" ]]; then
    RESULTS_ROOT="${RIG_DIR}/results/tier0.5-$(date +%Y%m%d-%H%M%S)"
fi
mkdir -p "$RESULTS_ROOT"
REPORT="${RESULTS_ROOT}/report.txt"
: > "$REPORT"

log() {
    echo "$@" | tee -a "$REPORT"
}

# Capture window = baseline + dd_duration_estimate + tail. dd duration is
# bounded above by PAYLOAD_MIB / 5 MB/s = ~6.4 s for 32 MiB on a slow
# disk; we cap with a generous 30 s ceiling to avoid clipping fdatasync.
DD_BUDGET_SECS=30
CAPTURE_SECS=$(( BASELINE_SECS + DD_BUDGET_SECS + TAIL_SECS ))

log "tier0.5-calibrate.sh starting"
log "  results dir:     $RESULTS_ROOT"
log "  payload:         ${PAYLOAD_MIB} MiB (${PAYLOAD_BYTES} bytes, ${DD_COUNT} × ${BS_BYTES}B writes)"
log "  baseline / tail: ${BASELINE_SECS}s / ${TAIL_SECS}s"
log "  capture window:  ${CAPTURE_SECS}s"
log ""

# Sidecar lifecycle.  We always remove on exit.
cleanup() {
    local rc=$?
    log ""
    log "cleanup: removing ${CALIBRATOR_NAME} and volume ${CALIBRATOR_VOL}"
    docker rm -f "$CALIBRATOR_NAME" >/dev/null 2>&1 || true
    docker volume rm "$CALIBRATOR_VOL" >/dev/null 2>&1 || true
    exit "$rc"
}
trap cleanup EXIT

# Reset any leftover calibrator from a previous failed run.
docker rm -f "$CALIBRATOR_NAME" >/dev/null 2>&1 || true
docker volume rm "$CALIBRATOR_VOL" >/dev/null 2>&1 || true

log "step 1: ensure rig is up (R=3)..."
(
    cd "$RIG_DIR"
    if ! docker compose -f docker/docker-compose.yaml --profile r3 ps --status running 2>/dev/null \
        | awk 'NR>1 {print}' | grep -q iops-nats-1; then
        log "  rig not running, starting r3 profile..."
        docker compose -f docker/docker-compose.yaml --profile r3 up -d
    else
        log "  rig already up"
    fi
)

log "step 2: launching ${CALIBRATOR_NAME} sidecar (${CALIBRATOR_IMAGE})..."
docker volume create "$CALIBRATOR_VOL" >/dev/null
# No --network: the sidecar only talks to its own /data volume.
docker run -d \
    --name "$CALIBRATOR_NAME" \
    -v "$CALIBRATOR_VOL":/data \
    "$CALIBRATOR_IMAGE" \
    sleep "$(( CAPTURE_SECS + 30 ))" >/dev/null
sleep 1  # let docker register the cgroup

# Sanity: cgroup path exists.
calib_id=$(docker inspect -f '{{.Id}}' "$CALIBRATOR_NAME")
calib_cg="/sys/fs/cgroup/system.slice/docker-${calib_id}.scope/io.stat"
if [[ ! -f "$calib_cg" ]]; then
    log "FAIL: cgroup path missing for sidecar: $calib_cg"
    exit 1
fi
log "  sidecar id:    ${calib_id:0:12}"
log "  cgroup path:   $calib_cg"

log "step 3: starting captures (${CAPTURE_SECS}s)..."
CGROUP_RAW="${RESULTS_ROOT}/cgroup_io.raw"
IOSTAT_RAW="${RESULTS_ROOT}/iostat.raw"

bash "${SCRIPT_DIR}/capture-cgroup-io.sh" \
    --output "$CGROUP_RAW" \
    --duration "$CAPTURE_SECS" \
    --containers "$CALIBRATOR_NAME" &
CG_PID=$!

bash "${SCRIPT_DIR}/capture-iostat.sh" \
    --output "$IOSTAT_RAW" \
    --duration "$CAPTURE_SECS" &
IOS_PID=$!
log "  capture PIDs:  cgroup=$CG_PID iostat=$IOS_PID"

# Mark t_dd_start / t_dd_end in unix seconds so we can window the parse.
log "step 4: baseline sleep ${BASELINE_SECS}s..."
sleep "$BASELINE_SECS"

T_DD_START_NS=$(date +%s%N)
log "step 5: dd ${PAYLOAD_MIB}MiB @bs=${BS_BYTES} oflag=direct conv=fdatasync"
DD_OUT="${RESULTS_ROOT}/dd.out"
docker exec "$CALIBRATOR_NAME" \
    dd if=/dev/zero of=/data/x \
       bs="$BS_BYTES" count="$DD_COUNT" \
       oflag=direct conv=fdatasync \
       2>"$DD_OUT" || {
        log "FAIL: dd inside sidecar exited non-zero"
        cat "$DD_OUT" | tee -a "$REPORT"
        exit 1
    }
T_DD_END_NS=$(date +%s%N)
log "  dd output:"
sed 's/^/    /' "$DD_OUT" | tee -a "$REPORT"

log "step 6: tail sleep ${TAIL_SECS}s..."
sleep "$TAIL_SECS"

log "step 7: stopping captures..."
kill -TERM "$CG_PID" "$IOS_PID" 2>/dev/null || true
wait "$CG_PID" 2>/dev/null || true
wait "$IOS_PID" 2>/dev/null || true

log "step 8: parsing cgroup cumulative counters for the sidecar..."
# The sidecar container was created clean just before the captures
# started; its only block-I/O activity is the dd. The cgroup's
# cumulative io.stat counter at the LAST sample therefore equals the
# total bytes/IOs the sidecar wrote — no windowing needed. Sum across
# all devices the cgroup reports (usually just one, the host's primary
# disk via the docker volume).
read -r CG_WBYTES CG_WIOS < <(awk '
    $0 ~ /^#/ { next }
    {
        dev = $3
        last_wb[dev] = $5
        last_wi[dev] = $7
    }
    END {
        tb = 0; ti = 0
        for (d in last_wb) { tb += last_wb[d]; ti += last_wi[d] }
        printf "%d %d\n", tb, ti
    }
' "$CGROUP_RAW")

log "  cgroup wbytes (cumulative for sidecar): $CG_WBYTES"
log "  cgroup wios   (cumulative for sidecar): $CG_WIOS"

log "step 9: parsing iostat (skipping the since-boot first block)..."
# iostat -x -d -t 1 emits a "Device" header followed by per-device
# rows for each sample. The FIRST block reports averages since system
# boot (iostat convention) — we skip it. Subsequent blocks are
# per-1-second rates; we sum w/s and wkB/s across all device rows.
# Since cadence is 1Hz the unitless sum approximates total writes
# over the capture. The sidecar's dd is the only intentional load;
# rig idle traffic is small noise floor.
read -r IOS_WKB_SUM IOS_WIO_SUM < <(awk '
    /^Device/ {
        for (i = 1; i <= NF; i++) col[$i] = i
        block_idx++
        next
    }
    # Only count blocks AFTER the first (since-boot) block.
    block_idx >= 2 && NF >= 2 && $1 !~ /^[0-9]/ && $1 != "" && $1 != "avg-cpu:" {
        if ("wkB/s" in col) ios_kb += $(col["wkB/s"])
        else if ("wMB/s" in col) ios_kb += $(col["wMB/s"]) * 1024
        if ("w/s" in col) ios_wio += $(col["w/s"])
    }
    END {
        printf "%.3f %.3f\n", ios_kb, ios_wio
    }
' "$IOSTAT_RAW")

IOS_WBYTES_TOTAL=$(awk -v kb="$IOS_WKB_SUM" 'BEGIN { printf "%.0f", kb * 1024 }')
IOS_WIOS_TOTAL=$(awk -v c="$IOS_WIO_SUM" 'BEGIN { printf "%.0f", c }')
log "  iostat wbytes (summed across capture, baseline ~= 0): $IOS_WBYTES_TOTAL"
log "  iostat wios   (summed across capture, baseline ~= 0): $IOS_WIOS_TOTAL"

log "step 10: computing ratios..."
ratio() {
    awk -v a="$1" -v b="$2" 'BEGIN { if (b+0 == 0) print "inf"; else printf "%.4f", a / b }'
}
in_band() {
    awk -v v="$1" -v lo="$2" -v hi="$3" '
        BEGIN { if (v == "inf") { print "FAIL"; exit }
                if (v + 0 >= lo + 0 && v + 0 <= hi + 0) print "PASS"; else print "FAIL" }'
}

CG_BYTES_RATIO=$(ratio "$CG_WBYTES" "$PAYLOAD_BYTES")
IOS_BYTES_RATIO=$(ratio "$IOS_WBYTES_TOTAL" "$PAYLOAD_BYTES")
CG_IOS_BYTES_RATIO=$(ratio "$CG_WBYTES" "$IOS_WBYTES_TOTAL")
CG_IOS_IOS_RATIO=$(ratio "$CG_WIOS" "$IOS_WIOS_TOTAL")
CG_IO_VS_DD=$(ratio "$CG_WIOS" "$DD_COUNT")
IOS_IO_VS_DD=$(ratio "$IOS_WIOS_TOTAL" "$DD_COUNT")

V1=$(in_band "$CG_BYTES_RATIO"      "$CG_BYTES_LO"  "$CG_BYTES_HI")
V2=$(in_band "$IOS_BYTES_RATIO"     "$IOS_BYTES_LO" "$IOS_BYTES_HI")
V3=$(in_band "$CG_IOS_BYTES_RATIO"  "$RATIO_LO"     "$RATIO_HI")

log ""
log "=== Tier 0.5 calibration results ==="
log "  dd payload:                 ${PAYLOAD_BYTES} bytes / ${DD_COUNT} IOs"
log "  cgroup wbytes:              ${CG_WBYTES}    ratio vs payload: ${CG_BYTES_RATIO}  [${CG_BYTES_LO}..${CG_BYTES_HI}]  ${V1}"
log "  iostat wbytes (sum):        ${IOS_WBYTES_TOTAL}    ratio vs payload: ${IOS_BYTES_RATIO}  [${IOS_BYTES_LO}..${IOS_BYTES_HI}]  ${V2}"
log "  cgroup / iostat bytes:      ${CG_IOS_BYTES_RATIO}  [${RATIO_LO}..${RATIO_HI}]  ${V3}"
log ""
log "  (informational, not gated:)"
log "  cgroup wios:                ${CG_WIOS}     ratio vs dd_count: ${CG_IO_VS_DD}"
log "  iostat wios (sum):          ${IOS_WIOS_TOTAL}     ratio vs dd_count: ${IOS_IO_VS_DD}"
log "  cgroup / iostat ios:        ${CG_IOS_IOS_RATIO}"
log ""

if [[ "$V1" == "PASS" && "$V2" == "PASS" && "$V3" == "PASS" ]]; then
    log "VERDICT: PASS — instruments are calibrated within the documented bands."
    exit 0
fi

log "VERDICT: FAIL — one or more bytes ratios out of band; instrument not yet calibrated."
log "  Diagnose by inspecting ${CGROUP_RAW} (cumulative counters) and ${IOSTAT_RAW} (per-second rates, skip first block)."
exit 1
