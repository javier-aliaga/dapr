/*
Copyright 2026 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package workflow

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dapr/dapr/tests/integration/framework"
	"github.com/dapr/dapr/tests/integration/framework/process/daprd"
	"github.com/dapr/dapr/tests/integration/framework/process/exec"
	"github.com/dapr/dapr/tests/integration/framework/process/workflow"
	"github.com/dapr/dapr/tests/integration/suite"
	dworkflow "github.com/dapr/durabletask-go/workflow"
)

func init() {
	suite.Register(new(eventloss))
}

const (
	// eventlossInstances is the number of agent-loop workflow instances
	// driven concurrently. The event-loss race is probabilistic
	// (independently observed at roughly 1-in-6 single-instance runs with
	// the Java SDK agent), so many concurrent instances make a single test
	// run reproduce it with high probability.
	eventlossInstances = 30

	// eventlossEventsPerInstance is the number of "agent-event" external
	// events raised to each instance before the terminating "done" event.
	eventlossEventsPerInstance = 16

	// eventlossLLMLatency is the simulated latency of the "llm-call"
	// activity. Real latency is load-bearing: the next external event must
	// arrive while the activity is in flight, so its EventRaised lands in
	// the workflow inbox alongside the activity's TaskCompleted and the two
	// are consumed as one batch — the window in which the event can be
	// eaten without completing the waiter it was destined for.
	eventlossLLMLatency = 20 * time.Millisecond

	// eventlossTimeout is the deadline for every instance to reach
	// COMPLETED. A healthy run finishes well within it; instances that hit
	// the race stay RUNNING forever, so a longer wait would not save them.
	// The integration framework caps each test case at 45s, so this must
	// leave room for setup and teardown.
	eventlossTimeout = 35 * time.Second

	// eventlossEmptyInboxLog is the daprd debug-level signature of the lost
	// event: a new-event reminder fires but the message it announced is
	// gone from the inbox, so the run request is ignored and the workflow
	// stalls in RUNNING.
	eventlossEmptyInboxLog = "because the workflow inbox is empty"
)

// eventloss reproduces a workflow event-loss race in the actor workflow
// backend on a single healthy daprd — no fault injection, no placement
// rebalance — by mirroring the agentic workload that hits it in the wild
// (a langchain4j agent on the Java SDK):
//
//   - the workflow waits for external events with a TIMEOUT, so every wait
//     arms a durable timer (CreateTimer) that is cancelled when the event
//     arrives — exactly what the Java SDK's waitForExternalEvent compiles
//     to;
//   - each event triggers activities with real latency (an LLM call), so
//     the NEXT event is raised while an activity is in flight and lands in
//     the same inbox batch as the activity's TaskCompleted;
//   - the wait is re-armed in the same turn that schedules the next
//     activity, producing the [CreateTimer#n, ScheduleTask#n+1] action
//     pairs seen in the failing traces.
//
// In the failing trace the batch [TaskCompleted#13, EventRaised] returns
// only [CreateTimer#15]: the raised event is consumed from the inbox but
// never completes the waiter, the subsequent new-event-er reminder fires
// against an empty inbox and is acked away, and the workflow stays RUNNING
// forever although the application-side orchestrator has nothing left to
// do. The orchestrator triggering side effects before committing its own
// state is what lets the in-flight save race the concurrently delivered
// event.
type eventloss struct {
	workflow  *workflow.Workflow
	daprdLogs *eventlossBuffer

	mu         sync.Mutex
	llmStarted map[string]chan struct{}
}

// eventlossBuffer is a concurrency-safe capture of daprd's stdout, used for
// the secondary (non-gating) diagnostic of the empty-inbox log signature.
type eventlossBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *eventlossBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *eventlossBuffer) Close() error { return nil }

func (b *eventlossBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (e *eventloss) Setup(t *testing.T) []framework.Option {
	e.daprdLogs = new(eventlossBuffer)

	e.workflow = workflow.New(t,
		workflow.WithDaprdOptions(0,
			daprd.WithLogLevel("debug"),
			daprd.WithExecOptions(exec.WithStdout(e.daprdLogs)),
		),
	)

	return []framework.Option{
		framework.WithProcesses(e.workflow),
	}
}

func (e *eventloss) Run(t *testing.T, ctx context.Context) {
	e.workflow.WaitUntilRunning(t, ctx)

	instanceIDs := make([]string, eventlossInstances)
	e.llmStarted = make(map[string]chan struct{}, eventlossInstances)
	for i := range eventlossInstances {
		instanceIDs[i] = fmt.Sprintf("evtloss-%d", i)
		// Generous buffer: the activity may execute more than once under
		// internal retries and the signal send must never block it.
		e.llmStarted[instanceIDs[i]] = make(chan struct{}, eventlossEventsPerInstance*4)
	}

	reg := dworkflow.NewRegistry()

	// The agent loop. The event wait uses a (long) timeout so each wait
	// arms a durable timer, and is re-armed BEFORE awaiting the activities
	// so the re-arm's CreateTimer and the llm-call's ScheduleTask are
	// emitted by the same turn, as in the failing traces.
	require.NoError(t, reg.AddWorkflowN("agentloop", func(wctx *dworkflow.WorkflowContext) (any, error) {
		var instanceID string
		if err := wctx.GetInput(&instanceID); err != nil {
			return nil, err
		}
		pending := wctx.WaitForExternalEvent("agent-event", 24*time.Hour)
		for {
			var payload string
			if err := pending.Await(&payload); err != nil {
				return nil, err
			}
			if payload == "done" {
				return "agent-done", nil
			}
			pending = wctx.WaitForExternalEvent("agent-event", 24*time.Hour)
			if err := wctx.CallActivity("llm-call",
				dworkflow.WithActivityInput(instanceID+"|"+payload),
			).Await(nil); err != nil {
				return nil, err
			}
			if err := wctx.CallActivity("tool-call",
				dworkflow.WithActivityInput(payload),
			).Await(nil); err != nil {
				return nil, err
			}
		}
	}))

	require.NoError(t, reg.AddActivityN("llm-call", func(actx dworkflow.ActivityContext) (any, error) {
		var input string
		if err := actx.GetInput(&input); err != nil {
			return nil, err
		}
		instanceID, _, _ := strings.Cut(input, "|")
		// Wait out the latency, then signal the driver JUST BEFORE
		// returning: the next event is raised right at the completion
		// boundary, so its EventRaised races the TaskCompleted's inbox
		// append. The two land in one batch, the event's own new-event
		// reminder goes stale, fires on an empty inbox and deactivates the
		// actor — and the next delivery races the deactivate/recreate.
		// NOTE: latency must stay uniform and short; the rc.3 regression
		// window needs rapid uniform cycling (a mixed slow/fast profile
		// was empirically unable to reproduce it).
		select {
		case <-actx.Context().Done():
			return nil, actx.Context().Err()
		case <-time.After(eventlossLLMLatency):
		}
		e.mu.Lock()
		ch := e.llmStarted[instanceID]
		e.mu.Unlock()
		select {
		case ch <- struct{}{}:
		default:
		}
		return "llm-ok", nil
	}))

	require.NoError(t, reg.AddActivityN("tool-call", func(actx dworkflow.ActivityContext) (any, error) {
		return "tool-ok", nil
	}))

	client := dworkflow.NewClient(e.workflow.Dapr().GRPCConn(t, ctx))
	require.NoError(t, client.StartWorker(ctx, reg))

	driveCtx, cancel := context.WithTimeout(ctx, eventlossTimeout)
	defer cancel()

	// retry re-attempts an API call until it succeeds or the drive deadline
	// expires, as a real client would on a transient error. Each attempt
	// gets its own short timeout so a server-side hang surfaces as a
	// retried attempt instead of silently eating the whole drive budget.
	// What the test asserts is that an ACCEPTED event is never lost
	// afterwards.
	retry := func(op string, fn func(ctx context.Context) error) error {
		for {
			attemptCtx, attemptCancel := context.WithTimeout(driveCtx, 5*time.Second)
			err := fn(attemptCtx)
			attemptCancel()
			if err == nil {
				return nil
			}
			if driveCtx.Err() != nil {
				return fmt.Errorf("%s: %w (last error: %s)", op, driveCtx.Err(), err)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}

	// Drive every instance concurrently: start it, then feed it events,
	// raising the next event as soon as the current llm-call activity has
	// started so the event arrives while the activity is in flight.
	errCh := make(chan error, eventlossInstances)
	var wg sync.WaitGroup
	for i := range eventlossInstances {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()

			if err := retry("schedule "+id, func(actx context.Context) error {
				_, serr := client.ScheduleWorkflow(actx, "agentloop",
					dworkflow.WithInstanceID(id),
					dworkflow.WithInput(id),
				)
				// A previous attempt that timed out client-side may have
				// landed server-side; the instance existing IS success.
				if serr != nil && strings.Contains(serr.Error(), "already exists") {
					return nil
				}
				return serr
			}); err != nil {
				errCh <- err
				return
			}

			e.mu.Lock()
			llm := e.llmStarted[id]
			e.mu.Unlock()

			for j := range eventlossEventsPerInstance {
				if err := retry(fmt.Sprintf("raise agent-event %d to %s", j, id), func(actx context.Context) error {
					return client.RaiseEvent(actx, id, "agent-event",
						dworkflow.WithEventPayload(fmt.Sprintf("evt-%d", j)))
				}); err != nil {
					errCh <- err
					return
				}
				// Wait for the event's llm-call to run. If the accepted
				// event was lost the agent loop never advances: record it
				// and stop driving this instance — the completion sweep
				// below reports its stuck runtime status.
				select {
				case <-llm:
				case <-time.After(10 * time.Second):
					errCh <- fmt.Errorf("agent-event %d accepted by %s but its llm-call never ran", j, id)
					return
				case <-driveCtx.Done():
					errCh <- fmt.Errorf("llm-call %d for %s never started: %w", j, id, driveCtx.Err())
					return
				}
			}
			if err := retry("raise done to "+id, func(actx context.Context) error {
				return client.RaiseEvent(actx, id, "agent-event",
					dworkflow.WithEventPayload("done"))
			}); err != nil {
				errCh <- err
			}
		}(instanceIDs[i])
	}
	wg.Wait()
	close(errCh)
	var driveErrs []string
	for err := range errCh {
		driveErrs = append(driveErrs, err.Error())
	}

	// Primary assertion: every instance reaches COMPLETED before the
	// deadline. Drive errors (a raised event whose llm-call never ran) are
	// folded into the final report next to the stuck statuses they cause.
	pending := make(map[string]bool, eventlossInstances)
	for _, id := range instanceIDs {
		pending[id] = true
	}
	for len(pending) > 0 && driveCtx.Err() == nil {
		for id := range pending {
			md, err := client.FetchWorkflowMetadata(driveCtx, id)
			if err == nil && md.RuntimeStatus == dworkflow.StatusCompleted {
				delete(pending, id)
			}
		}
		if len(pending) > 0 {
			time.Sleep(250 * time.Millisecond)
		}
	}

	// Secondary diagnostic (non-gating): the lost-event log signature.
	logs := e.daprdLogs.String()
	if path := os.Getenv("EVENTLOSS_DEBUG_LOG"); path != "" {
		require.NoError(t, os.WriteFile(path, []byte(logs), 0o600))
	}
	ignored := strings.Count(logs, eventlossEmptyInboxLog)
	t.Logf("daprd ignored %d new-event reminder(s) due to an empty inbox", ignored)

	if len(pending) == 0 && len(driveErrs) == 0 {
		return
	}

	// Report every stuck instance with its runtime status.
	stuck := make([]string, 0, len(pending))
	for id := range pending {
		stuck = append(stuck, id)
	}
	sort.Strings(stuck)

	report := make([]string, 0, len(stuck))
	for _, id := range stuck {
		status := "<unknown>"
		if md, err := client.FetchWorkflowMetadata(ctx, id); err != nil {
			status = fmt.Sprintf("<%v>", err)
		} else {
			status = md.String()
		}
		report = append(report, fmt.Sprintf("%s: %s", id, status))
	}

	require.Failf(t, "workflow instances did not complete",
		"%d/%d instances stuck after %s (daprd empty-inbox reminder ignores: %d):\n%s\ndrive errors:\n%s",
		len(stuck), eventlossInstances, eventlossTimeout, ignored,
		strings.Join(report, "\n"), strings.Join(driveErrs, "\n"))
}
