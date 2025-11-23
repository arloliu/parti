# Quick Verification Guide

## Option 1: Automated Test Script (Recommended)

Run the quick verification script:
```bash
cd test/simulation
./quick-test.sh
```

This will:
- Build the binary
- Run a 2-minute simulation with chaos
- Verify no message loss/duplication
- Show color-coded results

**Expected output:**
```
✓ Simulation started
✓ NATS server initialized
✓ Chaos controller started
✓ Chaos events injected: 2-3 events
✓ Reports generated: 4 reports
✓✓✓ SUCCESS: No message loss or duplication!
```

## Option 2: Manual Quick Test

### Step 1: Build
```bash
cd test/simulation
go build -o bin/simulation cmd/simulation/main.go
```

### Step 2: Run 2-minute test
```bash
./bin/simulation -config configs/quick-test.yaml
```

### Step 3: Watch the output for:
1. **Startup logs:**
   ```
   Starting simulation in all-in-one mode
   Chaos controller started (events: [worker_crash worker_restart], interval: 30s-45s)
   ```

2. **Periodic reports every 30 seconds:**
   ```
   === Simulation Report ===
   Total Partitions: 10
   Total Sent:       600
   Total Received:   600
   Gaps Detected:    0
   Duplicates:       0
   SUCCESS: No message loss or duplication detected
   ```

3. **Chaos events (1-2 times):**
   ```
   [Chaos] Injecting event: worker_crash
   [Chaos] Handling event: worker_crash
   [Chaos] Crashing worker: worker-1
   ```

### Step 4: Verify Success
After 2 minutes, check the final report:
- ✅ **Gaps Detected: 0**
- ✅ **Duplicates: 0**
- ✅ **Total Sent ≈ Total Received** (±10 messages is OK during shutdown)

## Option 3: Ultra-Fast Dev Test (30 seconds)

For rapid iteration:
```bash
# Create a 30-second test config
cat > configs/dev-test.yaml << EOF
simulation:
  duration: 30s
  mode: all-in-one

partitions:
  count: 5
  message_rate_per_partition: 2.0
  distribution: uniform

producers:
  count: 1
  partitions_per_producer: 5

workers:
  count: 2
  assignment_strategy: ConsistentHash
  processing_delay:
    min: 5ms
    max: 20ms

chaos:
  enabled: false

nats:
  mode: embedded

metrics:
  enabled: false

checkpoint:
  enabled: false
EOF

# Run it
./bin/simulation -config configs/dev-test.yaml
```

## Troubleshooting

### If build fails:
```bash
go mod tidy
go build -o bin/simulation cmd/simulation/main.go
```

### If NATS connection fails:
- The embedded NATS should work automatically
- Check if port 4222 is available: `lsof -i :4222`

### To see detailed logs:
```bash
./bin/simulation -config configs/quick-test.yaml 2>&1 | tee test.log
```

### To test WITHOUT chaos (simpler):
Edit `configs/quick-test.yaml`:
```yaml
chaos:
  enabled: false
```

## What to Look For

### ✅ Working Correctly:
- "SUCCESS: No message loss or duplication detected"
- Gaps Detected: 0
- Duplicates: 0
- Messages flowing (sent count increasing)

### ❌ Problems:
- "FAILURE: Message loss or duplication detected"
- Gaps > 0 (messages lost)
- Duplicates > 0 (messages delivered twice)
- No messages sent/received

### ⚠️ Expected Chaos Behavior:
- Worker crashes: Normal, workers restart
- Temporary message delays: Normal during chaos
- Final report should still show 0 gaps/duplicates

## Performance Expectations

With `quick-test.yaml`:
- **Message rate:** ~10 msg/sec
- **Total messages (2 min):** ~1200 messages
- **Chaos events:** 2-3 events
- **Reports:** 4 reports (every 30 seconds)

## Next Steps

Once quick test passes:
1. **Test realistic config (8 hours):**
   ```bash
   ./bin/simulation -config configs/realistic.yaml
   ```

2. **Test with stress config:**
   ```bash
   ./bin/simulation -config configs/stress.yaml
   ```

3. **Monitor with Prometheus** (if enabled in config)
