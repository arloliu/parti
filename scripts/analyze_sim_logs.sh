#!/usr/bin/env bash
set -euo pipefail

DIR="${1:-}"
if [[ -z "${DIR}" ]]; then
  echo "Usage: $0 <logs_dir>" >&2
  exit 2
fi

echo "== Path check =="
if [[ -d "$DIR" ]]; then echo "OK: $DIR exists"; else echo "MISSING: $DIR"; exit 1; fi

echo
echo "== Top-level listing =="
ls -lah "$DIR" || true

echo
echo "== File inventory (maxdepth=2) =="
find "$DIR" -maxdepth 2 -type f -printf "%TY-%Tm-%Td %TH:%TM  %9s  %p\n" | sort || true

echo
echo "== CSV files (recursive) =="
mapfile -t CSVs < <(find "$DIR" -type f -iname "*.csv" -print 2>/dev/null | sort)
if (( ${#CSVs[@]} == 0 )); then
  echo "No CSV files found."
else
  for f in "${CSVs[@]}"; do
    echo "---- ${f}"
    if [[ -s "$f" ]]; then
      echo "Rows: $(wc -l < "$f")"
      echo "Header:"
      head -n1 "$f" | sed -E 's/,/ | /g'
      echo "First 3 data rows:"
      tail -n +2 "$f" | head -n 3
      echo "Last 3 rows:"
      tail -n 3 "$f"
    else
      echo "File is empty"
    fi
    echo
  done
fi

echo
echo "== Log-like files (nohup,out,log) =="
mapfile -t LOGS < <(find "$DIR" -type f \( -iname "*.log" -o -iname "*.out" -o -iname "nohup" -o -iname "nohup.out" \) -print 2>/dev/null | sort)
if (( ${#LOGS[@]} == 0 )); then
  echo "No log files found."
else
  printf "%s\n" "${LOGS[@]}" | while read -r p; do
    size=$(stat -c %s "$p" 2>/dev/null || stat -f %z "$p" 2>/dev/null || echo 0)
    printf "%9s  %s\n" "$size" "$p"
  done | sort -nr | head -n 3 | awk '{print $2}' | while read -r lf; do
    echo "---- ${lf} (last 200 lines)"
    tail -n 200 "$lf" || true
    echo
  done
fi

echo
echo "== Any JSON summaries =="
mapfile -t JSONS < <(find "$DIR" -type f -iname "*.json" -print 2>/dev/null | sort)
if (( ${#JSONS[@]} == 0 )); then
  echo "No JSON files found."
else
  for jf in "${JSONS[@]}"; do
    echo "---- ${jf} (first 40 lines)"
    head -n 40 "$jf" || true
    echo
  done
fi
