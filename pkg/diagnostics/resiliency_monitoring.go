package diagnostics

import (
	"context"

	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"

	diagUtils "github.com/dapr/dapr/pkg/diagnostics/utils"
	"github.com/dapr/dapr/pkg/resiliency/breaker"
)

var (
	CircuitBreakerPolicy PolicyType = "circuitbreaker"
	RetryPolicy          PolicyType = "retry"
	TimeoutPolicy        PolicyType = "timeout"

	OutboundPolicyFlowDirection PolicyFlowDirection = "outbound"
	InboundPolicyFlowDirection  PolicyFlowDirection = "inbound"

	cbStatuses = []string{
		string(breaker.StateClosed),
		string(breaker.StateHalfOpen),
		string(breaker.StateOpen),
		string(breaker.StateUnknown),
	}
)

type PolicyType string

type PolicyFlowDirection string

type resiliencyMetrics struct {
	policiesLoadCount   *stats.Int64Measure
	executionCount      *stats.Int64Measure
	activationsCount    *stats.Int64Measure
	circuitbreakerState *stats.Int64Measure

	appID   string
	ctx     context.Context
	enabled bool
	meter   stats.Recorder
}

func newResiliencyMetrics() *resiliencyMetrics {
	return &resiliencyMetrics{ //nolint:exhaustruct
		policiesLoadCount: stats.Int64(
			"resiliency/loaded",
			"Number of resiliency policies loaded.",
			stats.UnitDimensionless),
		executionCount: stats.Int64(
			"resiliency/count",
			"Number of times a resiliency policyKey has been applied to a building block.",
			stats.UnitDimensionless),
		activationsCount: stats.Int64(
			"resiliency/activations_total",
			"Number of times a resiliency policyKey has been activated in a building block after a failure or after a state change.",
			stats.UnitDimensionless),
		circuitbreakerState: stats.Int64(
			"resiliency/cb_state",
			"A resiliency policy's current CircuitBreakerState state. 0 is closed, 1 is half-open, 2 is open, and -1 is unknown.",
			stats.UnitDimensionless),
		// TODO: how to use correct context
		ctx:     context.Background(),
		enabled: false,
	}
}

// Init registers the resiliency metrics views.
func (m *resiliencyMetrics) Init(meter view.Meter, id string) error {
	m.enabled = true
	m.appID = id
	m.meter = meter

	return meter.Register(
		diagUtils.NewMeasureView(m.policiesLoadCount, []tag.Key{appIDKey, resiliencyNameKey, namespaceKey}, view.Count()),
		diagUtils.NewMeasureView(m.executionCount, []tag.Key{appIDKey, resiliencyNameKey, policyKey, namespaceKey, flowDirectionKey, targetKey, statusKey}, view.Count()),
		diagUtils.NewMeasureView(m.activationsCount, []tag.Key{appIDKey, resiliencyNameKey, policyKey, namespaceKey, flowDirectionKey, targetKey, statusKey}, view.Count()),
		diagUtils.NewMeasureView(m.circuitbreakerState, []tag.Key{appIDKey, resiliencyNameKey, policyKey, namespaceKey, flowDirectionKey, targetKey, statusKey}, view.LastValue()),
	)
}

// PolicyLoaded records metric when policy is loaded.
func (m *resiliencyMetrics) PolicyLoaded(resiliencyName, namespace string) {
	if m.enabled {
		_ = stats.RecordWithOptions(
			m.ctx,
			stats.WithRecorder(m.meter),
			stats.WithTags(diagUtils.WithTags(m.policiesLoadCount.Name(), appIDKey, m.appID, resiliencyNameKey, resiliencyName, namespaceKey, namespace)...),
			stats.WithMeasurements(m.policiesLoadCount.M(1)),
		)
	}
}

// PolicyWithStatusExecuted records metric when policy is executed with added status information (e.g., circuit breaker open).
func (m *resiliencyMetrics) PolicyWithStatusExecuted(resiliencyName, namespace string, policy PolicyType, flowDirection PolicyFlowDirection, target string, status string) {
	if m.enabled {
		// Common tags for all metrics
		commonTags := []interface{}{
			appIDKey, m.appID,
			resiliencyNameKey, resiliencyName,
			policyKey, string(policy),
			namespaceKey, namespace,
			flowDirectionKey, string(flowDirection),
			targetKey, target,
			statusKey, // status appened on each recording
		}

		// Record count metric for all resiliency executions
		_ = stats.RecordWithOptions(
			m.ctx,
			stats.WithRecorder(m.meter),
			stats.WithTags(diagUtils.WithTags(m.executionCount.Name(), append(commonTags, status)...)...),
			stats.WithMeasurements(m.executionCount.M(1)),
		)

		// Record cb gauge, 4 metrics, one for each cb state, with the active state having a value of 1, otherwise 0
		if policy == CircuitBreakerPolicy {
			for _, s := range cbStatuses {
				if s == status {
					_ = stats.RecordWithOptions(
						m.ctx,
						stats.WithRecorder(m.meter),
						stats.WithTags(diagUtils.WithTags(m.circuitbreakerState.Name(), append(commonTags, s)...)...),
						stats.WithMeasurements(m.circuitbreakerState.M(1)),
					)
				} else {
					_ = stats.RecordWithOptions(
						m.ctx,
						stats.WithRecorder(m.meter),
						stats.WithTags(diagUtils.WithTags(m.circuitbreakerState.Name(), append(commonTags, s)...)...),
						stats.WithMeasurements(m.circuitbreakerState.M(0)),
					)
				}
			}
		}
	}
}

// PolicyExecuted records metric when policy is executed.
func (m *resiliencyMetrics) PolicyExecuted(resiliencyName, namespace string, policy PolicyType, flowDirection PolicyFlowDirection, target string) {
	m.PolicyWithStatusExecuted(resiliencyName, namespace, policy, flowDirection, target, "")
}

// PolicyActivated records metric when policy is activated after a failure
func (m *resiliencyMetrics) PolicyActivated(resiliencyName, namespace string, policy PolicyType, flowDirection PolicyFlowDirection, target string) {
	m.PolicyWithStatusActivated(resiliencyName, namespace, policy, flowDirection, target, "")
}

// PolicyWithStatusActivated records metrics when policy is activated after a failure or in the case of circuit breaker after a state change. with added state/status (e.g., circuit breaker open).
func (m *resiliencyMetrics) PolicyWithStatusActivated(resiliencyName, namespace string, policy PolicyType, flowDirection PolicyFlowDirection, target string, status string) {
	if m.enabled {
		// Record combined activation measure
		_ = stats.RecordWithOptions(
			m.ctx,
			stats.WithRecorder(m.meter),
			stats.WithTags(diagUtils.WithTags(m.activationsCount.Name(), appIDKey, m.appID, resiliencyNameKey, resiliencyName, policyKey, string(policy),
				namespaceKey, namespace, flowDirectionKey, string(flowDirection), targetKey, target, statusKey, status)...),
			stats.WithMeasurements(m.activationsCount.M(1)),
		)
	}
}

func ResiliencyActorTarget(actorType string) string {
	return "actor_" + actorType
}

func ResiliencyAppTarget(app string) string {
	return "app_" + app
}

func ResiliencyComponentTarget(name string, componentType string) string {
	return componentType + "_" + name
}
