# APISIX Gateway Configuration Reference

This document provides detailed APISIX Gateway configuration for implementing tool-based traffic routing in the tFDC deployment strategy.

## Table of Contents

- [Overview](#overview)
- [Gateway Deployment](#gateway-deployment)
- [Route Configuration](#route-configuration)
- [Plugin Configuration](#plugin-configuration)
- [Upstream Management](#upstream-management)
- [Health Checks](#health-checks)
- [Monitoring and Observability](#monitoring-and-observability)

## Overview

### Native gRPC Proxying

APISIX supports **native gRPC proxying** without any transcoding plugins. When the upstream `scheme` is set to `grpc`, APISIX automatically:
- Routes gRPC requests directly to gRPC backends
- Preserves gRPC protocol semantics (streaming, metadata, etc.)
- Applies plugins that operate on gRPC headers and metadata
- Performs health checks using gRPC health check protocol

**Data Flow:**
```
gRPC Client → APISIX (gRPC Proxy) → gRPC Backend
```

**For tFDC:** Since data collectors already speak gRPC, we use native proxying - no transcoding needed.

## Gateway Deployment

### APISIX Installation (Kubernetes)

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: apisix
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: apisix-config
  namespace: apisix
data:
  config.yaml: |
    apisix:
      node_listen: 9080
      enable_admin: true
      admin_key:
        - name: admin
          key: $APISIX_ADMIN_KEY
          role: admin
    etcd:
      host:
        - "http://etcd.apisix.svc.cluster.local:2379"
      prefix: "/apisix"
      timeout: 30
    plugin_attr:
      prometheus:
        export_addr:
          ip: "0.0.0.0"
          port: 9091
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: apisix
  namespace: apisix
  labels:
    app: apisix
spec:
  replicas: 3
  selector:
    matchLabels:
      app: apisix
  template:
    metadata:
      labels:
        app: apisix
    spec:
      containers:
      - name: apisix
        image: apache/apisix:3.8.0
        ports:
        - name: http
          containerPort: 9080
        - name: admin
          containerPort: 9180
        - name: metrics
          containerPort: 9091
        volumeMounts:
        - name: config
          mountPath: /usr/local/apisix/conf/config.yaml
          subPath: config.yaml
        resources:
          requests:
            cpu: 500m
            memory: 512Mi
          limits:
            cpu: 2000m
            memory: 2Gi
        livenessProbe:
          httpGet:
            path: /apisix/status
            port: 9080
          initialDelaySeconds: 10
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /apisix/status
            port: 9080
          initialDelaySeconds: 5
          periodSeconds: 5
      volumes:
      - name: config
        configMap:
          name: apisix-config
---
apiVersion: v1
kind: Service
metadata:
  name: apisix-gateway
  namespace: apisix
spec:
  type: LoadBalancer
  selector:
    app: apisix
  ports:
  - name: http
    port: 80
    targetPort: 9080
  - name: admin
    port: 9180
    targetPort: 9180
---
apiVersion: v1
kind: Service
metadata:
  name: apisix-admin
  namespace: apisix
spec:
  type: ClusterIP
  selector:
    app: apisix
  ports:
  - name: admin
    port: 9180
    targetPort: 9180
```

## Route Configuration

### Main Data Collector Route

```yaml
apiVersion: apisix.apache.org/v2
kind: ApisixRoute
metadata:
  name: dc-route
  namespace: tfdc
  annotations:
    argo-rollouts.argoproj.io/managed: "true"
spec:
  http:
  - name: data-collector-grpc
    match:
      hosts:
      - dc.tfdc.svc.cluster.local
      - dc.example.com
      paths:
      - /*
      methods:
      - POST
      - GET
    backends:
    - serviceName: dc-blue
      servicePort: 50051
      weight: 100
    plugins:
    - name: traffic-split
      enable: true
      config:
        rules: []  # Dynamically updated by tFDC Management plugin

    - name: tfdc-tool-router
      enable: true
      config:
        management_api: "http://tfdc-management.tfdc.svc.cluster.local:8080"
        routing_endpoint: "/api/v1/routing/canary"
        cache_ttl: 60
        default_route: "blue"

    - name: prometheus
      enable: true
      config:
        prefer_name: true

    - name: request-id
      enable: true
      config:
        header_name: "X-Request-ID"
        include_in_response: true
```

### Dynamic Traffic Splitting

```yaml
# This configuration is managed by Argo Rollouts and tFDC Management
# Example of traffic-split rules structure
#
# Note: http_tool_id is the NGINX variable for the "tool-id" HTTP header
# APISIX converts "tool-id" → "http_tool_id" (http_ prefix + hyphen to underscore)
traffic-split:
  rules:
  - match:
      - vars:
          # Tools explicitly routed to canary
          - ["http_tool_id", "in", ["tool_001", "tool_002", "tool_050"]]
    weighted_upstreams:
    - upstream:
        name: dc-green
        pass_host: pass
      weight: 100

  - match:
      - vars:
          # Pattern-based routing for tool categories
          - ["http_tool_id", "~~", "^tool_exp_.*"]
    weighted_upstreams:
    - upstream:
        name: dc-green
        pass_host: pass
      weight: 100

  - match:
      - vars:
          # Percentage-based routing (10% of remaining tools)
          - ["http_tool_id", "~~", "^tool_[0-9]{3}$"]
          - ["http_tool_id", "%", "10", "==", "0"]
    weighted_upstreams:
    - upstream:
        name: dc-green
        pass_host: pass
      weight: 100
```

## Plugin Configuration

### Custom tFDC Tool Router Plugin

```lua
-- Custom APISIX plugin for tFDC tool-based routing
-- File: /usr/local/apisix/plugins/tfdc-tool-router.lua

local core = require("apisix.core")
local http = require("resty.http")
local lrucache = require("resty.lrucache")

local plugin_name = "tfdc-tool-router"

local schema = {
    type = "object",
    properties = {
        management_api = {
            type = "string",
            description = "tFDC Management API endpoint"
        },
        routing_endpoint = {
            type = "string",
            default = "/api/v1/routing/canary"
        },
        cache_ttl = {
            type = "integer",
            default = 60,
            minimum = 10
        },
        default_route = {
            type = "string",
            enum = {"blue", "green"},
            default = "blue"
        }
    },
    required = {"management_api"}
}

local _M = {
    version = 1.0,
    priority = 1000,
    name = plugin_name,
    schema = schema,
}

-- LRU cache for routing decisions (1000 tools max)
local routing_cache = lrucache.new(1000)

function _M.check_schema(conf)
    return core.schema.check(schema, conf)
end

function _M.access(conf, ctx)
    -- Extract tool-id from header
    local tool_id = core.request.header(ctx, "tool-id")

    if not tool_id then
        core.log.warn("Missing tool-id header, using default route: ", conf.default_route)
        ctx.var.upstream_name = "dc-" .. conf.default_route
        return
    end

    -- Check cache first
    local cache_key = "routing:" .. tool_id
    local cached_route = routing_cache:get(cache_key)

    if cached_route then
        ctx.var.upstream_name = "dc-" .. cached_route
        return
    end

    -- Fetch from tFDC Management API
    local httpc = http.new()
    httpc:set_timeout(1000)  -- 1 second timeout

    local res, err = httpc:request_uri(
        conf.management_api .. conf.routing_endpoint,
        {
            method = "GET",
            headers = {
                ["Content-Type"] = "application/json",
            },
            query = {
                tool_id = tool_id
            }
        }
    )

    if not res or err then
        core.log.error("Failed to fetch routing from Management API: ", err)
        -- Fallback to default route
        ctx.var.upstream_name = "dc-" .. conf.default_route
        return
    end

    -- Parse response
    local routing_data, decode_err = core.json.decode(res.body)
    if not routing_data or decode_err then
        core.log.error("Failed to decode routing response: ", decode_err)
        ctx.var.upstream_name = "dc-" .. conf.default_route
        return
    end

    -- Determine route (check if tool is in canary list)
    local target_route = conf.default_route
    if routing_data.tools then
        for _, tool in ipairs(routing_data.tools) do
            if tool == tool_id then
                target_route = "green"
                break
            end
        end
    end

    -- Cache the decision
    routing_cache:set(cache_key, target_route, conf.cache_ttl)

    -- Set upstream
    ctx.var.upstream_name = "dc-" .. target_route

    -- Add routing metadata to response headers (for debugging)
    core.response.set_header("X-Routing-Target", target_route)
    core.response.set_header("X-Routing-Cache", "miss")
end

return _M
```

### Request/Response Logging Plugin

```yaml
plugins:
- name: http-logger
  enable: true
  config:
    uri: "http://log-collector.monitoring.svc.cluster.local:8080/logs"
    timeout: 3
    batch_max_size: 100
    inactive_timeout: 5
    buffer_duration: 60
    include_req_body: false
    include_resp_body: false
    concat_method: "json"
```

### Rate Limiting Plugin

```yaml
plugins:
- name: limit-req
  enable: true
  config:
    rate: 1000  # requests per second
    burst: 2000
    key_type: "var"
    key: "tool_id"
    rejected_code: 429
    rejected_msg: "Tool request rate limit exceeded"
    allow_degradation: false
```

## Upstream Management

### Blue Upstream Configuration

```yaml
apiVersion: apisix.apache.org/v2
kind: ApisixUpstream
metadata:
  name: dc-blue
  namespace: tfdc
spec:
  scheme: grpc
  loadbalancer:
    type: roundrobin
  healthCheck:
    active:
      type: grpc
      timeout: 1
      concurrency: 10
      healthy:
        interval: 2
        successes: 2
      unhealthy:
        interval: 1
        grpc_failures: 2
        http_failures: 2
    passive:
      healthy:
        successes: 3
      unhealthy:
        grpc_failures: 3
        http_failures: 3
  portLevelSettings:
  - port: 50051
    scheme: grpc
  timeout:
    connect: 60
    send: 60
    read: 60
  retries: 2
  retry_timeout: 0
---
apiVersion: v1
kind: Service
metadata:
  name: dc-blue
  namespace: tfdc
spec:
  type: ClusterIP
  selector:
    app: data-collector
    deployment-color: blue
  ports:
  - name: grpc
    port: 50051
    targetPort: 50051
    protocol: TCP
```

### Green Upstream Configuration

```yaml
apiVersion: apisix.apache.org/v2
kind: ApisixUpstream
metadata:
  name: dc-green
  namespace: tfdc
spec:
  scheme: grpc
  loadbalancer:
    type: roundrobin
  healthCheck:
    active:
      type: grpc
      timeout: 1
      concurrency: 10
      healthy:
        interval: 2
        successes: 2
      unhealthy:
        interval: 1
        grpc_failures: 2
        http_failures: 2
    passive:
      healthy:
        successes: 3
      unhealthy:
        grpc_failures: 3
        http_failures: 3
  portLevelSettings:
  - port: 50051
    scheme: grpc
  timeout:
    connect: 60
    send: 60
    read: 60
  retries: 2
  retry_timeout: 0
---
apiVersion: v1
kind: Service
metadata:
  name: dc-green
  namespace: tfdc
spec:
  type: ClusterIP
  selector:
    app: data-collector
    deployment-color: green
  ports:
  - name: grpc
    port: 50051
    targetPort: 50051
    protocol: TCP
```

## Health Checks

### gRPC Health Check Configuration

```yaml
healthCheck:
  active:
    type: grpc
    timeout: 1
    concurrency: 10
    grpc_service: ""  # Empty for gRPC health check protocol
    grpc_status: 0     # OK status
    healthy:
      interval: 2      # Check every 2 seconds
      successes: 2     # 2 consecutive successes to mark healthy
    unhealthy:
      interval: 1      # Check every 1 second when unhealthy
      grpc_failures: 2 # 2 consecutive failures to mark unhealthy
      timeouts: 2      # 2 timeouts to mark unhealthy
```

### Passive Health Check

```yaml
healthCheck:
  passive:
    type: grpc
    healthy:
      successes: 3     # 3 successes in window to mark healthy
    unhealthy:
      grpc_failures: 3 # 3 failures in window to mark unhealthy
      timeouts: 3      # 3 timeouts in window to mark unhealthy
```

## Monitoring and Observability

### Prometheus Metrics Configuration

```yaml
plugins:
- name: prometheus
  enable: true
  config:
    prefer_name: true  # Use route name instead of URI in metrics
    export_addr:
      ip: "0.0.0.0"
      port: 9091
```

### Key Metrics Exposed

```
# Request metrics
apisix_http_status{code="200", route="dc-route"} 15234
apisix_http_latency_bucket{route="dc-route", le="0.1"} 12000
apisix_http_latency_bucket{route="dc-route", le="0.5"} 15000
apisix_http_latency_sum{route="dc-route"} 3456.78
apisix_http_latency_count{route="dc-route"} 15234

# Upstream metrics
apisix_upstream_status{upstream="dc-blue", code="200"} 8000
apisix_upstream_status{upstream="dc-green", code="200"} 7234

# Bandwidth metrics
apisix_bandwidth{route="dc-route", type="ingress"} 123456789
apisix_bandwidth{route="dc-route", type="egress"} 987654321

# Connection metrics
apisix_nginx_http_current_connections{state="active"} 150
apisix_nginx_http_current_connections{state="reading"} 10
apisix_nginx_http_current_connections{state="writing"} 20
```

### Grafana Dashboard

```json
{
  "dashboard": {
    "title": "APISIX - tFDC Traffic Routing",
    "panels": [
      {
        "title": "Requests by Upstream",
        "targets": [
          {
            "expr": "sum(rate(apisix_http_status[5m])) by (upstream)"
          }
        ]
      },
      {
        "title": "Traffic Split Ratio",
        "targets": [
          {
            "expr": "sum(rate(apisix_http_status{upstream=\"dc-blue\"}[5m])) / sum(rate(apisix_http_status[5m]))"
          },
          {
            "expr": "sum(rate(apisix_http_status{upstream=\"dc-green\"}[5m])) / sum(rate(apisix_http_status[5m]))"
          }
        ]
      },
      {
        "title": "P95 Latency by Upstream",
        "targets": [
          {
            "expr": "histogram_quantile(0.95, sum(rate(apisix_http_latency_bucket[5m])) by (upstream, le))"
          }
        ]
      },
      {
        "title": "Error Rate by Upstream",
        "targets": [
          {
            "expr": "sum(rate(apisix_http_status{code=~\"5..\"}[5m])) by (upstream) / sum(rate(apisix_http_status[5m])) by (upstream)"
          }
        ]
      }
    ]
  }
}
```

## CLI Operations

### APISIX Admin API

```bash
# Set admin key
export APISIX_ADMIN_KEY="your-admin-key"

# List all routes
curl "http://apisix-admin.apisix.svc.cluster.local:9180/apisix/admin/routes" \
  -H "X-API-KEY: $APISIX_ADMIN_KEY"

# Get specific route
curl "http://apisix-admin.apisix.svc.cluster.local:9180/apisix/admin/routes/dc-route" \
  -H "X-API-KEY: $APISIX_ADMIN_KEY"

# Update route (traffic split)
curl "http://apisix-admin.apisix.svc.cluster.local:9180/apisix/admin/routes/dc-route" \
  -H "X-API-KEY: $APISIX_ADMIN_KEY" \
  -X PATCH \
  -d '{
    "plugins": {
      "traffic-split": {
        "rules": [
          {
            "match": [
              {
                "vars": [
                  ["http_tool_id", "in", ["tool_001", "tool_002"]]
                ]
              }
            ],
            "weighted_upstreams": [
              {
                "upstream": {"name": "dc-green", "pass_host": "pass"},
                "weight": 100
              }
            ]
          }
        ]
      }
    }
  }'

# List upstreams
curl "http://apisix-admin.apisix.svc.cluster.local:9180/apisix/admin/upstreams" \
  -H "X-API-KEY: $APISIX_ADMIN_KEY"

# Check upstream health
curl "http://apisix-admin.apisix.svc.cluster.local:9180/apisix/admin/upstreams/dc-blue/health" \
  -H "X-API-KEY: $APISIX_ADMIN_KEY"
```

### Testing Traffic Routing

```bash
# Send test request to blue
curl -X POST http://apisix-gateway.apisix.svc.cluster.local/submit \
  -H "tool-id: tool_001" \
  -H "Content-Type: application/json" \
  -d '{"data": "test"}'

# Check routing headers in response
curl -v -X POST http://apisix-gateway.apisix.svc.cluster.local/submit \
  -H "tool-id: tool_001" \
  -H "Content-Type: application/json" \
  -d '{"data": "test"}' \
  2>&1 | grep "X-Routing-Target"
```

## Traffic Migration Scripts

### Progressive Tool Migration

```bash
#!/bin/bash
# Script to progressively migrate tools to canary

MANAGEMENT_API="http://tfdc-management.tfdc.svc.cluster.local:8080"
TOOLS_FILE="tools-to-migrate.txt"

# Read tools from file
while IFS= read -r tool_id; do
    echo "Migrating $tool_id to canary..."

    # Update tFDC Management
    curl -X POST "$MANAGEMENT_API/api/v1/routing/update" \
      -H "Content-Type: application/json" \
      -d "{\"action\": \"expand_canary\", \"tools\": [\"$tool_id\"]}"

    # Wait for propagation
    sleep 5

    # Verify routing
    response=$(curl -s "$MANAGEMENT_API/api/v1/routing/canary")
    if echo "$response" | grep -q "$tool_id"; then
        echo "✓ $tool_id successfully routed to canary"
    else
        echo "✗ Failed to route $tool_id"
        exit 1
    fi

    # Wait before next tool
    sleep 30
done < "$TOOLS_FILE"

echo "Migration complete!"
```

### Instant Rollback

```bash
#!/bin/bash
# Script for instant rollback to stable

MANAGEMENT_API="http://tfdc-management.tfdc.svc.cluster.local:8080"

echo "Rolling back all tools to stable..."

curl -X POST "$MANAGEMENT_API/api/v1/routing/update" \
  -H "Content-Type: application/json" \
  -d '{"action": "rollback", "tools": []}'

echo "✓ Rollback complete - all traffic routed to stable"
```

## Troubleshooting

### Check Route Configuration

```bash
# View current route config
kubectl get apisixroute dc-route -n tfdc -o yaml

# Check APISIX logs
kubectl logs -l app=apisix -n apisix --tail=100 -f

# View access logs
kubectl exec -it -n apisix deploy/apisix -- tail -f /usr/local/apisix/logs/access.log
```

### Common Issues

1. **Tool routing not working**
   ```bash
   # Check if tool-id header is present
   # Verify tFDC Management API is accessible
   curl http://tfdc-management.tfdc.svc.cluster.local:8080/api/v1/routing/canary

   # Clear routing cache
   kubectl exec -it -n apisix deploy/apisix -- \
     curl -X POST http://localhost:9180/apisix/admin/plugins/reload \
     -H "X-API-KEY: $APISIX_ADMIN_KEY"
   ```

2. **Upstream health checks failing**
   ```bash
   # Check upstream status
   kubectl get apisixupstream -n tfdc

   # Verify service endpoints
   kubectl get endpoints dc-blue dc-green -n tfdc
   ```

3. **High latency**
   ```bash
   # Check APISIX resource usage
   kubectl top pods -l app=apisix -n apisix

   # Review metrics
   curl http://apisix-gateway.apisix.svc.cluster.local:9091/apisix/prometheus/metrics
   ```

## References

- [APISIX Documentation](https://apisix.apache.org/docs/)
- [APISIX Plugin Development](https://apisix.apache.org/docs/apisix/plugin-develop/)
- [Traffic Split Plugin](https://apisix.apache.org/docs/apisix/plugins/traffic-split/)
