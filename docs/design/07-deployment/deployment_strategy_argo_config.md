# Argo Rollouts Configuration Reference

This document provides detailed Argo Rollouts configuration for implementing the Blue/Green with Tool-by-Tool Canary deployment strategy for tFDC.

## Table of Contents

- [Rollout Resource Definition](#rollout-resource-definition)
- [Analysis Templates](#analysis-templates)
- [Progressive Delivery Steps](#progressive-delivery-steps)
- [Metric Providers](#metric-providers)
- [Notification Configuration](#notification-configuration)

## Rollout Resource Definition

### Data Collector Rollout

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  name: tfdc-data-collector
  namespace: tfdc
  labels:
    app: data-collector
    component: tfdc
spec:
  replicas: 10
  revisionHistoryLimit: 3

  selector:
    matchLabels:
      app: data-collector

  template:
    metadata:
      labels:
        app: data-collector
        version: "{{ .Version }}"
    spec:
      containers:
      - name: data-collector
        image: tfdc/data-collector:v1.0.0
        ports:
        - name: grpc
          containerPort: 50051
          protocol: TCP
        - name: metrics
          containerPort: 9090
          protocol: TCP
        env:
        - name: NATS_URL
          valueFrom:
            configMapKeyRef:
              name: tfdc-nats-config
              key: nats-url
        - name: DEPLOYMENT_COLOR
          valueFrom:
            fieldRef:
              fieldPath: metadata.labels['rollout-color']
        - name: CASSANDRA_HOSTS
          valueFrom:
            configMapKeyRef:
              name: tfdc-cassandra-config
              key: hosts
        resources:
          requests:
            cpu: 500m
            memory: 1Gi
          limits:
            cpu: 2000m
            memory: 4Gi
        livenessProbe:
          grpc:
            port: 50051
          initialDelaySeconds: 10
          periodSeconds: 10
        readinessProbe:
          grpc:
            port: 50051
          initialDelaySeconds: 5
          periodSeconds: 5

  strategy:
    canary:
      # Use APISIX for traffic routing
      trafficRouting:
        plugins:
          tfdc-management:
            # Custom plugin integrating with tFDC Management
            apiEndpoint: "http://tfdc-management.tfdc.svc.cluster.local:8080"
            routingPath: "/api/v1/routing/canary"
            apisixRoute: "dc-route"
            apisixNamespace: "tfdc"

      # Canary analysis configuration
      analysis:
        templates:
        - templateName: tfdc-data-collector-analysis
        startingStep: 2
        args:
        - name: service-name
          value: data-collector
        - name: deployment-color
          valueFrom:
            fieldRef:
              fieldPath: metadata.labels['rollout-color']

      # Progressive rollout steps
      steps:
      # Step 1: Deploy canary with minimal replicas
      - setCanaryScale:
          replicas: 2
      - pause:
          duration: 2m

      # Step 2: Route 1-5 smoke test tools
      - setHeaderRoute:
          name: smoke-test
          match:
          - headerName: tool-id
            headerValue:
              prefix: "tool_smoke_"
      - analysis:
          templates:
          - templateName: tfdc-smoke-test-analysis
          args:
          - name: tool-pattern
            value: "tool_smoke_.*"
      - pause:
          duration: 5m

      # Step 3: Expand to 50 tools
      - setCanaryScale:
          replicas: 3
      - experiment:
          duration: 10m
          templates:
          - name: canary
            specRef: canary
            replicas: 3
          analyses:
          - name: tool-expansion
            templateName: tfdc-expansion-analysis
            args:
            - name: tool-count
              value: "50"
      - pause:
          duration: 10m

      # Step 4: 10% traffic
      - setWeight: 10
      - pause:
          duration: 10m

      # Step 5: 25% traffic with analysis
      - setWeight: 25
      - analysis:
          templates:
          - templateName: tfdc-data-collector-analysis
      - pause:
          duration: 10m

      # Step 6: 50% traffic
      - setWeight: 50
      - analysis:
          templates:
          - templateName: tfdc-full-analysis
          args:
          - name: minimum-success-rate
            value: "99.5"
      - pause:
          duration: 15m

      # Step 7: Full traffic
      - setWeight: 100
      - pause:
          duration: 30m

      # Step 8: Promote to stable
      - setCanaryScale:
          replicas: 10
```

### Defender Rollout

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  name: tfdc-defender
  namespace: tfdc
  labels:
    app: defender
    component: tfdc
spec:
  replicas: 30
  revisionHistoryLimit: 3

  selector:
    matchLabels:
      app: defender

  template:
    metadata:
      labels:
        app: defender
        version: "{{ .Version }}"
    spec:
      containers:
      - name: defender
        image: tfdc/defender:v1.0.0
        ports:
        - name: metrics
          containerPort: 9090
          protocol: TCP
        env:
        - name: NATS_URL
          valueFrom:
            configMapKeyRef:
              name: tfdc-nats-config
              key: nats-url
        - name: DEPLOYMENT_COLOR
          valueFrom:
            fieldRef:
              fieldPath: metadata.labels['rollout-color']
        - name: CASSANDRA_HOSTS
          valueFrom:
            configMapKeyRef:
              name: tfdc-cassandra-config
              key: hosts
        resources:
          requests:
            cpu: 1000m
            memory: 2Gi
          limits:
            cpu: 4000m
            memory: 8Gi
        livenessProbe:
          httpGet:
            path: /healthz
            port: 9090
          initialDelaySeconds: 30
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /ready
            port: 9090
          initialDelaySeconds: 15
          periodSeconds: 5

  strategy:
    canary:
      # Defender follows Data Collector deployment
      # Traffic routing controlled by which NATS cluster it connects to
      canaryService: defender-canary
      stableService: defender-stable

      analysis:
        templates:
        - templateName: tfdc-defender-analysis
        startingStep: 1
        args:
        - name: service-name
          value: defender
        - name: deployment-color
          valueFrom:
            fieldRef:
              fieldPath: metadata.labels['rollout-color']

      steps:
      - setCanaryScale:
          replicas: 5
      - pause:
          duration: 5m
      - setCanaryScale:
          replicas: 15
      - pause:
          duration: 10m
      - setCanaryScale:
          replicas: 30
      - pause:
          duration: 30m
```

## Analysis Templates

### Smoke Test Analysis

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AnalysisTemplate
metadata:
  name: tfdc-smoke-test-analysis
  namespace: tfdc
spec:
  args:
  - name: tool-pattern
    value: ".*"
  - name: deployment-color

  metrics:
  # Basic health check
  - name: error-rate
    interval: 30s
    count: 5
    successCondition: result < 0.01  # Less than 1%
    failureCondition: result > 0.05  # More than 5%
    provider:
      prometheus:
        address: http://prometheus.monitoring.svc.cluster.local:9090
        query: |
          sum(rate(
            grpc_server_handled_total{
              grpc_code!="OK",
              tool_id=~"{{args.tool-pattern}}",
              deployment_color="{{args.deployment-color}}"
            }[1m]
          )) /
          sum(rate(
            grpc_server_handled_total{
              tool_id=~"{{args.tool-pattern}}",
              deployment_color="{{args.deployment-color}}"
            }[1m]
          ))

  # Latency check
  - name: p95-latency
    interval: 30s
    count: 5
    successCondition: result < 500  # Less than 500ms
    failureCondition: result > 1000  # More than 1s
    provider:
      prometheus:
        address: http://prometheus.monitoring.svc.cluster.local:9090
        query: |
          histogram_quantile(0.95,
            sum(rate(
              grpc_server_handling_seconds_bucket{
                tool_id=~"{{args.tool-pattern}}",
                deployment_color="{{args.deployment-color}}"
              }[1m]
            )) by (le)
          ) * 1000
```

### Full Deployment Analysis

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AnalysisTemplate
metadata:
  name: tfdc-full-analysis
  namespace: tfdc
spec:
  args:
  - name: service-name
  - name: deployment-color
  - name: minimum-success-rate
    value: "99.0"

  metrics:
  # Service-level error rate
  - name: service-error-rate
    interval: 60s
    count: 10
    successCondition: result < 0.01
    failureCondition: result > 0.05
    provider:
      prometheus:
        address: http://prometheus.monitoring.svc.cluster.local:9090
        query: |
          sum(rate(
            grpc_server_handled_total{
              grpc_code!="OK",
              service="{{args.service-name}}",
              deployment_color="{{args.deployment-color}}"
            }[2m]
          )) /
          sum(rate(
            grpc_server_handled_total{
              service="{{args.service-name}}",
              deployment_color="{{args.deployment-color}}"
            }[2m]
          ))

  # NATS message processing success
  - name: nats-processing-success
    interval: 60s
    count: 10
    successCondition: result > 0.99
    failureCondition: result < 0.95
    provider:
      prometheus:
        address: http://prometheus.monitoring.svc.cluster.local:9090
        query: |
          sum(rate(
            defender_messages_processed_total{
              status="success",
              deployment_color="{{args.deployment-color}}"
            }[2m]
          )) /
          sum(rate(
            defender_messages_processed_total{
              deployment_color="{{args.deployment-color}}"
            }[2m]
          ))

  # Resource utilization
  - name: cpu-usage
    interval: 60s
    count: 10
    successCondition: result < 80  # Less than 80%
    failureCondition: result > 95   # More than 95%
    provider:
      prometheus:
        address: http://prometheus.monitoring.svc.cluster.local:9090
        query: |
          avg(rate(
            container_cpu_usage_seconds_total{
              pod=~"{{args.service-name}}-.*",
              container="{{args.service-name}}"
            }[2m]
          )) * 100

  # Memory usage
  - name: memory-usage
    interval: 60s
    count: 10
    successCondition: result < 80  # Less than 80%
    failureCondition: result > 90   # More than 90%
    provider:
      prometheus:
        address: http://prometheus.monitoring.svc.cluster.local:9090
        query: |
          avg(
            container_memory_working_set_bytes{
              pod=~"{{args.service-name}}-.*",
              container="{{args.service-name}}"
            }
          ) / avg(
            container_spec_memory_limit_bytes{
              pod=~"{{args.service-name}}-.*",
              container="{{args.service-name}}"
            }
          ) * 100

  # Custom business metrics via webhook
  - name: business-metrics-validation
    interval: 120s
    count: 5
    successCondition: result == "pass"
    failureCondition: result == "fail"
    provider:
      web:
        url: http://tfdc-metrics-validator.tfdc.svc.cluster.local:8080/validate
        method: POST
        headers:
        - key: Content-Type
          value: application/json
        jsonPath: "{$.status}"
        body: |
          {
            "service": "{{args.service-name}}",
            "deployment_color": "{{args.deployment-color}}",
            "minimum_success_rate": "{{args.minimum-success-rate}}"
          }
```

### Defender-Specific Analysis

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AnalysisTemplate
metadata:
  name: tfdc-defender-analysis
  namespace: tfdc
spec:
  args:
  - name: service-name
    value: defender
  - name: deployment-color

  metrics:
  # Leader election stability
  - name: leader-stability
    interval: 60s
    count: 10
    successCondition: result <= 1  # No more than 1 leader change
    failureCondition: result > 3   # More than 3 leader changes
    provider:
      prometheus:
        address: http://prometheus.monitoring.svc.cluster.local:9090
        query: |
          increase(
            defender_leader_changes_total{
              deployment_color="{{args.deployment-color}}"
            }[5m]
          )

  # Subject assignment coverage
  - name: subject-assignment-coverage
    interval: 60s
    count: 10
    successCondition: result >= 0.99  # 99% subjects assigned
    failureCondition: result < 0.95    # Less than 95% assigned
    provider:
      prometheus:
        address: http://prometheus.monitoring.svc.cluster.local:9090
        query: |
          sum(defender_assigned_subjects{
            deployment_color="{{args.deployment-color}}"
          }) /
          sum(defender_total_subjects{
            deployment_color="{{args.deployment-color}}"
          })

  # Message lag
  - name: nats-consumer-lag
    interval: 60s
    count: 10
    successCondition: result < 100  # Less than 100 messages behind
    failureCondition: result > 1000 # More than 1000 messages behind
    provider:
      prometheus:
        address: http://prometheus.monitoring.svc.cluster.local:9090
        query: |
          max(
            nats_consumer_num_pending{
              deployment_color="{{args.deployment-color}}"
            }
          )
```

## Notification Configuration

### Slack Notifications

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: argo-rollouts-notification-configmap
  namespace: tfdc
data:
  service.slack: |
    token: $slack-token

  template.rollout-started: |
    message: |
      🚀 Deployment started for {{.rollout.metadata.name}}
      New version: {{.rollout.spec.template.spec.containers[0].image}}
      Cluster: {{.rollout.metadata.labels.deployment-color}}

  template.rollout-step-completed: |
    message: |
      ✓ Step completed: {{.rollout.status.currentStepIndex}}/{{.rollout.spec.strategy.canary.steps | len}}
      Canary weight: {{.rollout.status.canary.weight}}%

  template.rollout-paused: |
    message: |
      ⏸️ Rollout paused for {{.rollout.metadata.name}}
      Current step: {{.rollout.status.currentStepIndex}}
      Reason: {{.rollout.status.message}}
      Resume: `kubectl argo rollouts promote {{.rollout.metadata.name}} -n {{.rollout.metadata.namespace}}`

  template.rollout-completed: |
    message: |
      ✅ Deployment completed successfully!
      Service: {{.rollout.metadata.name}}
      Version: {{.rollout.spec.template.spec.containers[0].image}}
      Duration: {{.rollout.status.completedAt | timeSince .rollout.status.startedAt}}

  template.rollout-failed: |
    message: |
      ❌ Deployment failed for {{.rollout.metadata.name}}
      Reason: {{.rollout.status.message}}
      Analysis runs: {{.rollout.status.canary.currentAnalysisRuns}}
      Action required: Investigate and decide to retry or abort

  template.rollout-aborted: |
    message: |
      🛑 Rollout aborted for {{.rollout.metadata.name}}
      Reverted to stable version
      Check logs for failure details

  trigger.on-rollout-started: |
    - when: rollout.status.phase == 'Progressing'
      oncePer: rollout.metadata.name
      send: [rollout-started]

  trigger.on-rollout-step-completed: |
    - when: rollout.status.currentStepIndex > 0
      send: [rollout-step-completed]

  trigger.on-rollout-paused: |
    - when: rollout.status.phase == 'Paused'
      send: [rollout-paused]

  trigger.on-rollout-completed: |
    - when: rollout.status.phase == 'Healthy'
      oncePer: rollout.metadata.generation
      send: [rollout-completed]

  trigger.on-rollout-failed: |
    - when: rollout.status.phase == 'Degraded'
      send: [rollout-failed]

  trigger.on-rollout-aborted: |
    - when: rollout.status.abort == true
      send: [rollout-aborted]
```

## CLI Commands

### Common Operations

```bash
# Get rollout status
kubectl argo rollouts get rollout tfdc-data-collector -n tfdc

# Watch rollout progress
kubectl argo rollouts get rollout tfdc-data-collector -n tfdc --watch

# Promote to next step
kubectl argo rollouts promote tfdc-data-collector -n tfdc

# Abort rollout and revert
kubectl argo rollouts abort tfdc-data-collector -n tfdc

# Retry a failed rollout
kubectl argo rollouts retry rollout tfdc-data-collector -n tfdc

# Set image for rollout
kubectl argo rollouts set image tfdc-data-collector \
  data-collector=tfdc/data-collector:v1.1.0 -n tfdc

# Restart rollout
kubectl argo rollouts restart tfdc-data-collector -n tfdc

# List all rollouts
kubectl argo rollouts list rollouts -n tfdc

# Get analysis run details
kubectl get analysisrun -n tfdc
kubectl describe analysisrun <name> -n tfdc
```

### Dashboard Access

```bash
# Start Argo Rollouts dashboard
kubectl argo rollouts dashboard

# Access at http://localhost:3100
```

## Integration with CI/CD

### GitHub Actions Example

```yaml
name: Deploy with Argo Rollouts
on:
  push:
    tags:
      - 'v*'

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout code
        uses: actions/checkout@v3

      - name: Set version
        id: version
        run: echo "VERSION=${GITHUB_REF#refs/tags/}" >> $GITHUB_OUTPUT

      - name: Update rollout image
        run: |
          kubectl argo rollouts set image tfdc-data-collector \
            data-collector=tfdc/data-collector:${{ steps.version.outputs.VERSION }} \
            -n tfdc

          kubectl argo rollouts set image tfdc-defender \
            defender=tfdc/defender:${{ steps.version.outputs.VERSION }} \
            -n tfdc

      - name: Wait for rollout
        run: |
          kubectl argo rollouts wait tfdc-data-collector \
            -n tfdc \
            --timeout 60m

          kubectl argo rollouts wait tfdc-defender \
            -n tfdc \
            --timeout 60m

      - name: Verify deployment
        run: |
          kubectl argo rollouts status tfdc-data-collector -n tfdc
          kubectl argo rollouts status tfdc-defender -n tfdc
```

## Troubleshooting

### Check Rollout Status

```bash
# Get detailed status
kubectl describe rollout tfdc-data-collector -n tfdc

# Check analysis runs
kubectl get analysisrun -n tfdc
kubectl logs -l analysisrun=<name> -n tfdc

# View events
kubectl get events -n tfdc --field-selector involvedObject.name=tfdc-data-collector
```

### Common Issues

1. **Analysis fails immediately**
   - Check Prometheus connectivity
   - Verify metric queries return data
   - Check analysis template arguments

2. **Rollout stuck in paused state**
   - Review analysis run status
   - Check metric thresholds
   - Manually promote if metrics are acceptable

3. **Traffic not routing correctly**
   - Verify APISIX integration
   - Check tFDC Management API responses
   - Review service mesh configuration

## References

- [Argo Rollouts Documentation](https://argoproj.github.io/argo-rollouts/)
- [Analysis Templates Guide](https://argoproj.github.io/argo-rollouts/features/analysis/)
- [Traffic Management](https://argoproj.github.io/argo-rollouts/features/traffic-management/)
