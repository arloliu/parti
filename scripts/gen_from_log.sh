#!/usr/bin/env bash
set -euo pipefail
RUN_DIR=${1:-}
if [[ -z "$RUN_DIR" ]]; then
  echo "Usage: $0 /path/to/run_dir" >&2
  exit 2
fi
LOG="$RUN_DIR/run.log"
if [[ ! -f "$LOG" ]]; then
  echo "run.log not found in $RUN_DIR" >&2
  exit 2
fi
TIMES_CSV="$RUN_DIR/timeseries.csv"
SUMMARY_CSV="$RUN_DIR/summary.csv"
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
{
  echo "metric,min,max,last"
  awk -F, 'NR>1{print $5}' "$TIMES_CSV" | awk 'NR==1{min=max=$1} {if($1<min)min=$1; if($1>max)max=$1; last=$1} END{printf("inflight,%s,%s,%s\n",min,max,last)}'
  awk -F, 'NR>1{print $10}' "$TIMES_CSV" | sed 's/,//g' | awk 'NR==1{min=max=$1} {if($1<min)min=$1; if($1>max)max=$1; last=$1} END{printf("goroutines,%s,%s,%s\n",min,max,last)}'
  awk -F, 'NR>1{print $11}' "$TIMES_CSV" | awk 'NR==1{min=max=$1} {if($1<min)min=$1; if($1>max)max=$1; last=$1} END{printf("memory_mib,%s,%s,%s\n",min,max,last)}'
  awk -F, 'NR>1{print $16}' "$TIMES_CSV" | awk 'NR==1{min=max=$1} {if($1<min)min=$1; if($1>max)max=$1; last=$1} END{printf("locality_ratio,%s,%s,%s\n",min,max,last)}'
  awk -F, 'NR>1{print $17}' "$TIMES_CSV" | awk 'NR==1{min=max=$1} {if($1<min)min=$1; if($1>max)max=$1; last=$1} END{printf("stable_locality_ratio,%s,%s,%s\n",min,max,last)}'
  awk -F, 'NR>1{print $7}' "$TIMES_CSV" | awk 'NR==1{min=max=$1} {if($1<min)min=$1; if($1>max)max=$1; last=$1} END{printf("gaps,%s,%s,%s\n",min,max,last)}'
  awk -F, 'NR>1{print $22}' "$TIMES_CSV" | awk 'NR==1{min=max=$1} {if($1<min)min=$1; if($1>max)max=$1; last=$1} END{printf("disorder_depth,%s,%s,%s\n",min,max,last)}'
  awk -F, 'NR>1{print $6}' "$TIMES_CSV" | awk 'NR==1{min=max=$1} {if($1<min)min=$1; if($1>max)max=$1; last=$1} END{printf("pending_holes,%s,%s,%s\n",min,max,last)}'
  awk -F, 'NR>1{last=$18} END{printf("moved_partitions_total,,,%s\n", last)}' "$TIMES_CSV"
  awk -F, 'NR>1{last=$20} END{printf("ownership_mismatches,,,%s\n", last)}' "$TIMES_CSV"
  awk -F, 'NR>1{last=$23} END{printf("holes_healed_total,,,%s\n", last)}' "$TIMES_CSV"
  awk -F, 'NR>1{last=$24} END{printf("healed_rate_per_sec,,,%s\n", last)}' "$TIMES_CSV"
} > "$SUMMARY_CSV"

echo "Wrote: $TIMES_CSV"
echo "Wrote: $SUMMARY_CSV"
