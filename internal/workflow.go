// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package internal

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uber-go/tally"
	s "go.uber.org/cadence/.gen/go/shared"
	"go.uber.org/cadence/internal/common"
	"go.uber.org/zap"
)

var (
	errDomainNotSet                  = errors.New("domain is not set")
	errWorkflowIDNotSet              = errors.New("workflowId is not set")
	errLocalActivityParamsBadRequest = errors.New("missing local activity parameters through context, check LocalActivityOptions")
	errActivityParamsBadRequest      = errors.New("missing activity parameters through context, check ActivityOptions")
	errWorkflowOptionBadRequest      = errors.New("missing workflow options through context, check WorkflowOptions")
	errSearchAttributesNotSet        = errors.New("search attributes is empty")
)

type (
	// Channel must be used instead of native go channel by workflow code.
	// Use workflow.NewChannel(ctx) method to create Channel instance.
	Channel interface {
		// Receive blocks until it receives a value, and then assigns the received value to the provided pointer.
		// Returns false when Channel is closed.
		// Parameter valuePtr is a pointer to the expected data structure to be received. For example:
		//  var v string
		//  c.Receive(ctx, &v)
		Receive(ctx Context, valuePtr interface{}) (more bool)

		// ReceiveAsync try to receive from Channel without blocking. If there is data available from the Channel, it
		// assign the data to valuePtr and returns true. Otherwise, it returns false immediately.
		ReceiveAsync(valuePtr interface{}) (ok bool)

		// ReceiveAsyncWithMoreFlag is same as ReceiveAsync with extra return value more to indicate if there could be
		// more value from the Channel. The more is false when Channel is closed.
		ReceiveAsyncWithMoreFlag(valuePtr interface{}) (ok bool, more bool)

		// Send blocks until the data is sent.
		Send(ctx Context, v interface{})

		// SendAsync try to send without blocking. It returns true if the data was sent, otherwise it returns false.
		SendAsync(v interface{}) (ok bool)

		// Close close the Channel, and prohibit subsequent sends.
		Close()
	}

	// Selector must be used instead of native go select by workflow code.
	// Use workflow.NewSelector(ctx) method to create a Selector instance.
	Selector interface {
		AddReceive(c Channel, f func(c Channel, more bool)) Selector
		AddSend(c Channel, v interface{}, f func()) Selector
		AddFuture(future Future, f func(f Future)) Selector
		AddDefault(f func())
		Select(ctx Context)
	}

	// WaitGroup must be used instead of native go sync.WaitGroup by
	// workflow code.  Use workflow.NewWaitGroup(ctx) method to create
	// a new WaitGroup instance
	WaitGroup interface {
		Add(delta int)
		Done()
		Wait(ctx Context)
	}

	// Future represents the result of an asynchronous computation.
	Future interface {
		// Get blocks until the future is ready. When ready it either returns non nil error or assigns result value to
		// the provided pointer.
		// Example:
		//  var v string
		//  if err := f.Get(ctx, &v); err != nil {
		//      return err
		//  }
		//
		// The valuePtr parameter can be nil when the encoded result value is not needed.
		// Example:
		//  err = f.Get(ctx, nil)
		Get(ctx Context, valuePtr interface{}) error

		// When true Get is guaranteed to not block
		IsReady() bool
	}

	// Settable is used to set value or error on a future.
	// See more: workflow.NewFuture(ctx).
	Settable interface {
		Set(value interface{}, err error)
		SetValue(value interface{})
		SetError(err error)
		Chain(future Future) // Value (or error) of the future become the same of the chained one.
	}

	// ChildWorkflowFuture represents the result of a child workflow execution
	ChildWorkflowFuture interface {
		Future
		// GetChildWorkflowExecution returns a future that will be ready when child workflow execution started. You can
		// get the WorkflowExecution of the child workflow from the future. Then you can use Workflow ID and RunID of
		// child workflow to cancel or send signal to child workflow.
		//  childWorkflowFuture := workflow.ExecuteChildWorkflow(ctx, child, ...)
		//  var childWE WorkflowExecution
		//  if err := childWorkflowFuture.GetChildWorkflowExecution().Get(&childWE); err == nil {
		//      // child workflow started, you can use childWE to get the WorkflowID and RunID of child workflow
		//  }
		GetChildWorkflowExecution() Future

		// SignalWorkflowByID sends a signal to the child workflow. This call will block until child workflow is started.
		SignalChildWorkflow(ctx Context, signalName string, data interface{}) Future
	}

	// WorkflowType identifies a workflow type.
	WorkflowType struct {
		Name string
	}

	// WorkflowExecution Details.
	WorkflowExecution struct {
		ID    string
		RunID string
	}

	// EncodedValue is type alias used to encapsulate/extract encoded result from workflow/activity.
	EncodedValue struct {
		value         []byte
		dataConverter DataConverter
	}
	// Version represents a change version. See GetVersion call.
	Version int

	// ChildWorkflowOptions stores all child workflow specific parameters that will be stored inside of a Context.
	// The current timeout resolution implementation is in seconds and uses math.Ceil(d.Seconds()) as the duration. But is
	// subjected to change in the future.
	ChildWorkflowOptions struct {
		// Domain of the child workflow.
		// Optional: the current workflow (parent)'s domain will be used if this is not provided.
		Domain string

		// WorkflowID of the child workflow to be scheduled.
		// Optional: an auto generated workflowID will be used if this is not provided.
		WorkflowID string

		// TaskList that the child workflow needs to be scheduled on.
		// Optional: the parent workflow task list will be used if this is not provided.
		TaskList string

		// ExecutionStartToCloseTimeout - The end to end timeout for the child workflow execution.
		// Mandatory: no default
		ExecutionStartToCloseTimeout time.Duration

		// TaskStartToCloseTimeout - The decision task timeout for the child workflow.
		// Optional: default is 10s if this is not provided (or if 0 is provided).
		TaskStartToCloseTimeout time.Duration

		// WaitForCancellation - Whether to wait for cancelled child workflow to be ended (child workflow can be ended
		// as: completed/failed/timedout/terminated/canceled)
		// Optional: default false
		WaitForCancellation bool

		// WorkflowIDReusePolicy - Whether server allow reuse of workflow ID, can be useful
		// for dedup logic if set to WorkflowIdReusePolicyRejectDuplicate
		WorkflowIDReusePolicy WorkflowIDReusePolicy

		// RetryPolicy specify how to retry child workflow if error happens.
		// Optional: default is no retry
		RetryPolicy *RetryPolicy

		// CronSchedule - Optional cron schedule for workflow. If a cron schedule is specified, the workflow will run
		// as a cron based on the schedule. The scheduling will be based on UTC time. Schedule for next run only happen
		// after the current run is completed/failed/timeout. If a RetryPolicy is also supplied, and the workflow failed
		// or timeout, the workflow will be retried based on the retry policy. While the workflow is retrying, it won't
		// schedule its next run. If next schedule is due while workflow is running (or retrying), then it will skip that
		// schedule. Cron workflow will not stop until it is terminated or cancelled (by returning cadence.CanceledError).
		// The cron spec is as following:
		// ┌───────────── minute (0 - 59)
		// │ ┌───────────── hour (0 - 23)
		// │ │ ┌───────────── day of the month (1 - 31)
		// │ │ │ ┌───────────── month (1 - 12)
		// │ │ │ │ ┌───────────── day of the week (0 - 6) (Sunday to Saturday)
		// │ │ │ │ │
		// │ │ │ │ │
		// * * * * *
		CronSchedule string

		// Memo - Optional non-indexed info that will be shown in list workflow.
		Memo map[string]interface{}

		// SearchAttributes - Optional indexed info that can be used in query of List/Scan/Count workflow APIs (only
		// supported when Cadence server is using ElasticSearch). The key and value type must be registered on Cadence server side.
		// Use GetSearchAttributes API to get valid key and corresponding value type.
		SearchAttributes map[string]interface{}

		// ParentClosePolicy - Optional policy to decide what to do for the child.
		// Default is Terminate (if onboarded to this feature)
		ParentClosePolicy ParentClosePolicy
	}
)

// RegisterWorkflowOptions consists of options for registering a workflow
type RegisterWorkflowOptions struct {
	Name string
}

// RegisterWorkflow - registers a workflow function with the framework.
// A workflow takes a cadence context and input and returns a (result, error) or just error.
// Examples:
//	func sampleWorkflow(ctx workflow.Context, input []byte) (result []byte, err error)
//	func sampleWorkflow(ctx workflow.Context, arg1 int, arg2 string) (result []byte, err error)
//	func sampleWorkflow(ctx workflow.Context) (result []byte, err error)
//	func sampleWorkflow(ctx workflow.Context, arg1 int) (result string, err error)
// Serialization of all primitive types, structures is supported ... except channels, functions, variadic, unsafe pointer.
// This method calls panic if workflowFunc doesn't comply with the expected format.
func RegisterWorkflow(workflowFunc interface{}) {
	RegisterWorkflowWithOptions(workflowFunc, RegisterWorkflowOptions{})
}

// RegisterWorkflowWithOptions registers the workflow function with options
// The user can use options to provide an external name for the workflow or leave it empty if no
// external name is required. This can be used as
//  client.RegisterWorkflow(sampleWorkflow, RegisterWorkflowOptions{})
//  client.RegisterWorkflow(sampleWorkflow, RegisterWorkflowOptions{Name: "foo"})
// A workflow takes a cadence context and input and returns a (result, error) or just error.
// Examples:
//	func sampleWorkflow(ctx workflow.Context, input []byte) (result []byte, err error)
//	func sampleWorkflow(ctx workflow.Context, arg1 int, arg2 string) (result []byte, err error)
//	func sampleWorkflow(ctx workflow.Context) (result []byte, err error)
//	func sampleWorkflow(ctx workflow.Context, arg1 int) (result string, err error)
// Serialization of all primitive types, structures is supported ... except channels, functions, variadic, unsafe pointer.
// This method calls panic if workflowFunc doesn't comply with the expected format.
func RegisterWorkflowWithOptions(workflowFunc interface{}, opts RegisterWorkflowOptions) {
	thImpl := getHostEnvironment()
	err := thImpl.RegisterWorkflowWithOptions(workflowFunc, opts)
	if err != nil {
		panic(err)
	}
}

// NewChannel create new Channel instance
func NewChannel(ctx Context) Channel {
	state := getState(ctx)
	state.dispatcher.channelSequence++
	return NewNamedChannel(ctx, fmt.Sprintf("chan-%v", state.dispatcher.channelSequence))
}

// NewNamedChannel create new Channel instance with a given human readable name.
// Name appears in stack traces that are blocked on this channel.
func NewNamedChannel(ctx Context, name string) Channel {
	env := getWorkflowEnvironment(ctx)
	return &channelImpl{name: name, dataConverter: getDataConverterFromWorkflowContext(ctx), scope: env.GetMetricsScope(), logger: env.GetLogger()}
}

// NewBufferedChannel create new buffered Channel instance
func NewBufferedChannel(ctx Context, size int) Channel {
	env := getWorkflowEnvironment(ctx)
	return &channelImpl{size: size, dataConverter: getDataConverterFromWorkflowContext(ctx), scope: env.GetMetricsScope(), logger: env.GetLogger()}
}

// NewNamedBufferedChannel create new BufferedChannel instance with a given human readable name.
// Name appears in stack traces that are blocked on this Channel.
func NewNamedBufferedChannel(ctx Context, name string, size int) Channel {
	env := getWorkflowEnvironment(ctx)
	return &channelImpl{name: name, size: size, dataConverter: getDataConverterFromWorkflowContext(ctx), scope: env.GetMetricsScope(), logger: env.GetLogger()}
}

// NewSelector creates a new Selector instance.
func NewSelector(ctx Context) Selector {
	state := getState(ctx)
	state.dispatcher.selectorSequence++
	return NewNamedSelector(ctx, fmt.Sprintf("selector-%v", state.dispatcher.selectorSequence))
}

// NewNamedSelector creates a new Selector instance with a given human readable name.
// Name appears in stack traces that are blocked on this Selector.
func NewNamedSelector(ctx Context, name string) Selector {
	return &selectorImpl{name: name}
}

// NewWaitGroup creates a new WaitGroup instance.
func NewWaitGroup(ctx Context) WaitGroup {
	f, s := NewFuture(ctx)
	return &waitGroupImpl{future: f, settable: s}
}

// Go creates a new coroutine. It has similar semantic to goroutine in a context of the workflow.
func Go(ctx Context, f func(ctx Context)) {
	state := getState(ctx)
	state.dispatcher.newCoroutine(ctx, f)
}

// GoNamed creates a new coroutine with a given human readable name.
// It has similar semantic to goroutine in a context of the workflow.
// Name appears in stack traces that are blocked on this Channel.
func GoNamed(ctx Context, name string, f func(ctx Context)) {
	state := getState(ctx)
	state.dispatcher.newNamedCoroutine(ctx, name, f)
}

// NewFuture creates a new future as well as associated Settable that is used to set its value.
func NewFuture(ctx Context) (Future, Settable) {
	impl := &futureImpl{channel: NewChannel(ctx).(*channelImpl)}
	return impl, impl
}

// ExecuteActivity requests activity execution in the context of a workflow.
// Context can be used to pass the settings for this activity.
// For example: task list that this need to be routed, timeouts that need to be configured.
// Use ActivityOptions to pass down the options.
//  ao := ActivityOptions{
// 	    TaskList: "exampleTaskList",
// 	    ScheduleToStartTimeout: 10 * time.Second,
// 	    StartToCloseTimeout: 5 * time.Second,
// 	    ScheduleToCloseTimeout: 10 * time.Second,
// 	    HeartbeatTimeout: 0,
// 	}
//	ctx := WithActivityOptions(ctx, ao)
// Or to override a single option
//  ctx := WithTaskList(ctx, "exampleTaskList")
// Input activity is either an activity name (string) or a function representing an activity that is getting scheduled.
// Input args are the arguments that need to be passed to the scheduled activity.
//
// If the activity failed to complete then the future get error would indicate the failure, and it can be one of
// CustomError, TimeoutError, CanceledError, PanicError, GenericError.
// You can cancel the pending activity using context(workflow.WithCancel(ctx)) and that will fail the activity with
// error CanceledError.
//
// ExecuteActivity returns Future with activity result or failure.
func ExecuteActivity(ctx Context, activity interface{}, args ...interface{}) Future {
	// Validate type and its arguments.
	dataConverter := getDataConverterFromWorkflowContext(ctx)
	future, settable := newDecodeFuture(ctx, activity)
	activityType, input, err := getValidatedActivityFunction(activity, args, dataConverter)
	if err != nil {
		settable.Set(nil, err)
		return future
	}
	// Validate context options.
	options := getActivityOptions(ctx)
	options, err = getValidatedActivityOptions(ctx)
	if err != nil {
		settable.Set(nil, err)
		return future
	}

	// Validate session state.
	if sessionInfo := getSessionInfo(ctx); sessionInfo != nil {
		isCreationActivity := isSessionCreationActivity(activity)
		if sessionInfo.sessionState == sessionStateFailed && !isCreationActivity {
			settable.Set(nil, ErrSessionFailed)
			return future
		}
		if sessionInfo.sessionState == sessionStateOpen && !isCreationActivity {
			// Use session tasklist
			oldTaskListName := options.TaskListName
			options.TaskListName = sessionInfo.tasklist
			defer func() {
				options.TaskListName = oldTaskListName
			}()
		}
	}

	// Retrieve headers from context to pass them on
	header := getHeadersFromContext(ctx)

	params := executeActivityParams{
		activityOptions: *options,
		ActivityType:    *activityType,
		Input:           input,
		DataConverter:   dataConverter,
		Header:          header,
	}

	ctxDone, cancellable := ctx.Done().(*channelImpl)
	cancellationCallback := &receiveCallback{}
	a := getWorkflowEnvironment(ctx).ExecuteActivity(params, func(r []byte, e error) {
		settable.Set(r, e)
		if cancellable {
			// future is done, we don't need the cancellation callback anymore.
			ctxDone.removeReceiveCallback(cancellationCallback)
		}
	})

	if cancellable {
		cancellationCallback.fn = func(v interface{}, more bool) bool {
			if ctx.Err() == ErrCanceled {
				getWorkflowEnvironment(ctx).RequestCancelActivity(a.activityID)
			}
			return false
		}
		_, ok, more := ctxDone.receiveAsyncImpl(cancellationCallback)
		if ok || !more {
			cancellationCallback.fn(nil, more)
		}
	}
	return future
}

// ExecuteLocalActivity requests to run a local activity. A local activity is like a regular activity with some key
// differences:
// * Local activity is scheduled and run by the workflow worker locally.
// * Local activity does not need Cadence server to schedule activity task and does not rely on activity worker.
// * No need to register local activity.
// * The parameter activity to ExecuteLocalActivity() must be a function.
// * Local activity is for short living activities (usually finishes within seconds).
// * Local activity cannot heartbeat.
//
// Context can be used to pass the settings for this local activity.
// For now there is only one setting for timeout to be set:
//  lao := LocalActivityOptions{
// 	    ScheduleToCloseTimeout: 5 * time.Second,
// 	}
//	ctx := WithLocalActivityOptions(ctx, lao)
// The timeout here should be relative shorter than the DecisionTaskStartToCloseTimeout of the workflow. If you need a
// longer timeout, you probably should not use local activity and instead should use regular activity. Local activity is
// designed to be used for short living activities (usually finishes within seconds).
//
// Input args are the arguments that will to be passed to the local activity. The input args will be hand over directly
// to local activity function without serialization/deserialization because we don't need to pass the input across process
// boundary. However, the result will still go through serialization/deserialization because we need to record the result
// as history to cadence server so if the workflow crashes, a different worker can replay the history without running
// the local activity again.
//
// If the activity failed to complete then the future get error would indicate the failure, and it can be one of
// CustomError, TimeoutError, CanceledError, PanicError, GenericError.
// You can cancel the pending activity by cancel the context(workflow.WithCancel(ctx)) and that will fail the activity
// with error CanceledError.
//
// ExecuteLocalActivity returns Future with local activity result or failure.
func ExecuteLocalActivity(ctx Context, activity interface{}, args ...interface{}) Future {
	future, settable := newDecodeFuture(ctx, activity)

	if err := validateFunctionArgs(activity, args, false); err != nil {
		settable.Set(nil, err)
		return future
	}
	options, err := getValidatedLocalActivityOptions(ctx)
	if err != nil {
		settable.Set(nil, err)
		return future
	}

	params := &executeLocalActivityParams{
		localActivityOptions: *options,
		ActivityFn:           activity,
		ActivityType:         lastPartOfName(getFunctionName(activity)),
		InputArgs:            args,
		WorkflowInfo:         GetWorkflowInfo(ctx),
		DataConverter:        getDataConverterFromWorkflowContext(ctx),
		ScheduledTime:        Now(ctx), // initial scheduled time
	}

	Go(ctx, func(ctx Context) {
		for {
			f := scheduleLocalActivity(ctx, params)
			var result []byte
			err := f.Get(ctx, &result)
			if retryErr, ok := err.(*needRetryError); ok && retryErr.Backoff > 0 {
				// Backoff for retry
				Sleep(ctx, retryErr.Backoff)
				// increase the attempt, and retry the local activity
				params.Attempt = retryErr.Attempt + 1
				continue
			}

			// not more retry, return whatever is received.
			settable.Set(result, err)
			return
		}
	})

	return future
}

type needRetryError struct {
	Backoff time.Duration
	Attempt int32
}

func (e *needRetryError) Error() string {
	return fmt.Sprintf("Retry backoff: %v, Attempt: %v", e.Backoff, e.Attempt)
}

func scheduleLocalActivity(ctx Context, params *executeLocalActivityParams) Future {
	f := &futureImpl{channel: NewChannel(ctx).(*channelImpl)}
	ctxDone, cancellable := ctx.Done().(*channelImpl)
	cancellationCallback := &receiveCallback{}
	la := getWorkflowEnvironment(ctx).ExecuteLocalActivity(*params, func(lar *localActivityResultWrapper) {
		if cancellable {
			// future is done, we don't need cancellation anymore
			ctxDone.removeReceiveCallback(cancellationCallback)
		}

		if lar.err == nil || lar.backoff <= 0 {
			f.Set(lar.result, lar.err)
			return
		}

		// set retry error, and it will be handled by workflow.ExecuteLocalActivity().
		f.Set(nil, &needRetryError{Backoff: lar.backoff, Attempt: lar.attempt})
		return
	})

	if cancellable {
		cancellationCallback.fn = func(v interface{}, more bool) bool {
			if ctx.Err() == ErrCanceled {
				getWorkflowEnvironment(ctx).RequestCancelLocalActivity(la.activityID)
			}
			return false
		}
		_, ok, more := ctxDone.receiveAsyncImpl(cancellationCallback)
		if ok || !more {
			cancellationCallback.fn(nil, more)
		}
	}

	return f
}

// ExecuteChildWorkflow requests child workflow execution in the context of a workflow.
// Context can be used to pass the settings for the child workflow.
// For example: task list that this child workflow should be routed, timeouts that need to be configured.
// Use ChildWorkflowOptions to pass down the options.
//  cwo := ChildWorkflowOptions{
// 	    ExecutionStartToCloseTimeout: 10 * time.Minute,
// 	    TaskStartToCloseTimeout: time.Minute,
// 	}
//  ctx := WithChildWorkflowOptions(ctx, cwo)
// Input childWorkflow is either a workflow name or a workflow function that is getting scheduled.
// Input args are the arguments that need to be passed to the child workflow function represented by childWorkflow.
// If the child workflow failed to complete then the future get error would indicate the failure and it can be one of
// CustomError, TimeoutError, CanceledError, GenericError.
// You can cancel the pending child workflow using context(workflow.WithCancel(ctx)) and that will fail the workflow with
// error CanceledError.
// ExecuteChildWorkflow returns ChildWorkflowFuture.
func ExecuteChildWorkflow(ctx Context, childWorkflow interface{}, args ...interface{}) ChildWorkflowFuture {
	mainFuture, mainSettable := newDecodeFuture(ctx, childWorkflow)
	executionFuture, executionSettable := NewFuture(ctx)
	result := &childWorkflowFutureImpl{
		decodeFutureImpl: mainFuture.(*decodeFutureImpl),
		executionFuture:  executionFuture.(*futureImpl),
	}
	workflowOptionsFromCtx := getWorkflowEnvOptions(ctx)
	dc := workflowOptionsFromCtx.dataConverter
	wfType, input, err := getValidatedWorkflowFunction(childWorkflow, args, dc)
	if err != nil {
		executionSettable.Set(nil, err)
		mainSettable.Set(nil, err)
		return result
	}
	options, err := getValidatedWorkflowOptions(ctx)
	if err != nil {
		executionSettable.Set(nil, err)
		mainSettable.Set(nil, err)
		return result
	}
	options.dataConverter = dc
	options.contextPropagators = workflowOptionsFromCtx.contextPropagators
	options.memo = workflowOptionsFromCtx.memo
	options.searchAttributes = workflowOptionsFromCtx.searchAttributes

	params := executeWorkflowParams{
		workflowOptions: *options,
		input:           input,
		workflowType:    wfType,
		header:          getWorkflowHeader(ctx, options.contextPropagators),
		scheduledTime:   Now(ctx), /* this is needed for test framework, and is not send to server */
	}

	var childWorkflowExecution *WorkflowExecution

	ctxDone, cancellable := ctx.Done().(*channelImpl)
	cancellationCallback := &receiveCallback{}
	err = getWorkflowEnvironment(ctx).ExecuteChildWorkflow(params, func(r []byte, e error) {
		mainSettable.Set(r, e)
		if cancellable {
			// future is done, we don't need cancellation anymore
			ctxDone.removeReceiveCallback(cancellationCallback)
		}
	}, func(r WorkflowExecution, e error) {
		if e == nil {
			childWorkflowExecution = &r
		}
		executionSettable.Set(r, e)
	})

	if err != nil {
		executionSettable.Set(nil, err)
		mainSettable.Set(nil, err)
		return result
	}

	if cancellable {
		cancellationCallback.fn = func(v interface{}, more bool) bool {
			if ctx.Err() == ErrCanceled && childWorkflowExecution != nil && !mainFuture.IsReady() {
				// child workflow started, and ctx cancelled
				getWorkflowEnvironment(ctx).RequestCancelChildWorkflow(*options.domain, childWorkflowExecution.ID)
			}
			return false
		}
		_, ok, more := ctxDone.receiveAsyncImpl(cancellationCallback)
		if ok || !more {
			cancellationCallback.fn(nil, more)
		}
	}

	return result
}

func getWorkflowHeader(ctx Context, ctxProps []ContextPropagator) *s.Header {
	header := &s.Header{
		Fields: make(map[string][]byte),
	}
	writer := NewHeaderWriter(header)
	for _, ctxProp := range ctxProps {
		ctxProp.InjectFromWorkflow(ctx, writer)
	}
	return header
}

// WorkflowInfo information about currently executing workflow
type WorkflowInfo struct {
	WorkflowExecution                   WorkflowExecution
	WorkflowType                        WorkflowType
	TaskListName                        string
	ExecutionStartToCloseTimeoutSeconds int32
	TaskStartToCloseTimeoutSeconds      int32
	Domain                              string
	Attempt                             int32 // Attempt starts from 0 and increased by 1 for every retry if retry policy is specified.
	lastCompletionResult                []byte
	CronSchedule                        *string
	ContinuedExecutionRunID             *string
	ParentWorkflowDomain                *string
	ParentWorkflowExecution             *WorkflowExecution
	Memo                                *s.Memo
	SearchAttributes                    *s.SearchAttributes
	BinaryChecksum                      *string
}

// GetWorkflowInfo extracts info of a current workflow from a context.
func GetWorkflowInfo(ctx Context) *WorkflowInfo {
	return getWorkflowEnvironment(ctx).WorkflowInfo()
}

// GetLogger returns a logger to be used in workflow's context
func GetLogger(ctx Context) *zap.Logger {
	return getWorkflowEnvironment(ctx).GetLogger()
}

// GetMetricsScope returns a metrics scope to be used in workflow's context
func GetMetricsScope(ctx Context) tally.Scope {
	return getWorkflowEnvironment(ctx).GetMetricsScope()
}

// Now returns the current time when the decision is started or replayed.
// The workflow needs to use this Now() to get the wall clock time instead of the Go lang library one.
func Now(ctx Context) time.Time {
	return getWorkflowEnvironment(ctx).Now()
}

// NewTimer returns immediately and the future becomes ready after the specified duration d. The workflow needs to use
// this NewTimer() to get the timer instead of the Go lang library one(timer.NewTimer()). You can cancel the pending
// timer by cancel the Context (using context from workflow.WithCancel(ctx)) and that will cancel the timer. After timer
// is canceled, the returned Future become ready, and Future.Get() will return *CanceledError.
// The current timer resolution implementation is in seconds and uses math.Ceil(d.Seconds()) as the duration. But is
// subjected to change in the future.
func NewTimer(ctx Context, d time.Duration) Future {
	future, settable := NewFuture(ctx)
	if d <= 0 {
		settable.Set(true, nil)
		return future
	}

	ctxDone, cancellable := ctx.Done().(*channelImpl)
	cancellationCallback := &receiveCallback{}
	t := getWorkflowEnvironment(ctx).NewTimer(d, func(r []byte, e error) {
		settable.Set(nil, e)
		if cancellable {
			// future is done, we don't need cancellation anymore
			ctxDone.removeReceiveCallback(cancellationCallback)
		}
	})

	if t != nil && cancellable {
		cancellationCallback.fn = func(v interface{}, more bool) bool {
			if !future.IsReady() {
				getWorkflowEnvironment(ctx).RequestCancelTimer(t.timerID)
			}
			return false
		}
		_, ok, more := ctxDone.receiveAsyncImpl(cancellationCallback)
		if ok || !more {
			cancellationCallback.fn(nil, more)
		}
	}
	return future
}

// Sleep pauses the current workflow for at least the duration d. A negative or zero duration causes Sleep to return
// immediately. Workflow code needs to use this Sleep() to sleep instead of the Go lang library one(timer.Sleep()).
// You can cancel the pending sleep by cancel the Context (using context from workflow.WithCancel(ctx)).
// Sleep() returns nil if the duration d is passed, or it returns *CanceledError if the ctx is canceled. There are 2
// reasons the ctx could be canceled: 1) your workflow code cancel the ctx (with workflow.WithCancel(ctx));
// 2) your workflow itself is canceled by external request.
// The current timer resolution implementation is in seconds and uses math.Ceil(d.Seconds()) as the duration. But is
// subjected to change in the future.
func Sleep(ctx Context, d time.Duration) (err error) {
	t := NewTimer(ctx, d)
	err = t.Get(ctx, nil)
	return
}

// RequestCancelExternalWorkflow can be used to request cancellation of an external workflow.
// Input workflowID is the workflow ID of target workflow.
// Input runID indicates the instance of a workflow. Input runID is optional (default is ""). When runID is not specified,
// then the currently running instance of that workflowID will be used.
// By default, the current workflow's domain will be used as target domain. However, you can specify a different domain
// of the target workflow using the context like:
//	ctx := WithWorkflowDomain(ctx, "domain-name")
// RequestCancelExternalWorkflow return Future with failure or empty success result.
func RequestCancelExternalWorkflow(ctx Context, workflowID, runID string) Future {
	ctx1 := setWorkflowEnvOptionsIfNotExist(ctx)
	options := getWorkflowEnvOptions(ctx1)
	future, settable := NewFuture(ctx1)

	if options.domain == nil || *options.domain == "" {
		settable.Set(nil, errDomainNotSet)
		return future
	}

	if workflowID == "" {
		settable.Set(nil, errWorkflowIDNotSet)
		return future
	}

	resultCallback := func(result []byte, err error) {
		settable.Set(result, err)
	}

	getWorkflowEnvironment(ctx).RequestCancelExternalWorkflow(
		*options.domain,
		workflowID,
		runID,
		resultCallback,
	)

	return future
}

// SignalExternalWorkflow can be used to send signal info to an external workflow.
// Input workflowID is the workflow ID of target workflow.
// Input runID indicates the instance of a workflow. Input runID is optional (default is ""). When runID is not specified,
// then the currently running instance of that workflowID will be used.
// By default, the current workflow's domain will be used as target domain. However, you can specify a different domain
// of the target workflow using the context like:
//	ctx := WithWorkflowDomain(ctx, "domain-name")
// SignalExternalWorkflow return Future with failure or empty success result.
func SignalExternalWorkflow(ctx Context, workflowID, runID, signalName string, arg interface{}) Future {
	childWorkflowOnly := false // this means we are not limited to child workflow
	return signalExternalWorkflow(ctx, workflowID, runID, signalName, arg, childWorkflowOnly)
}

func signalExternalWorkflow(ctx Context, workflowID, runID, signalName string, arg interface{}, childWorkflowOnly bool) Future {
	ctx1 := setWorkflowEnvOptionsIfNotExist(ctx)
	options := getWorkflowEnvOptions(ctx1)
	future, settable := NewFuture(ctx1)

	if options.domain == nil || *options.domain == "" {
		settable.Set(nil, errDomainNotSet)
		return future
	}

	if workflowID == "" {
		settable.Set(nil, errWorkflowIDNotSet)
		return future
	}

	input, err := encodeArg(options.dataConverter, arg)
	if err != nil {
		settable.Set(nil, err)
		return future
	}

	resultCallback := func(result []byte, err error) {
		settable.Set(result, err)
	}
	getWorkflowEnvironment(ctx).SignalExternalWorkflow(
		*options.domain,
		workflowID,
		runID,
		signalName,
		input,
		arg,
		childWorkflowOnly,
		resultCallback,
	)

	return future
}

// UpsertSearchAttributes is used to add or update workflow search attributes.
// The search attributes can be used in query of List/Scan/Count workflow APIs.
// The key and value type must be registered on cadence server side;
// The value has to deterministic when replay;
// The value has to be Json serializable.
// UpsertSearchAttributes will merge attributes to existing map in workflow, for example workflow code:
//   func MyWorkflow(ctx workflow.Context, input string) error {
//	   attr1 := map[string]interface{}{
//		   "CustomIntField": 1,
//		   "CustomBoolField": true,
//	   }
//	   workflow.UpsertSearchAttributes(ctx, attr1)
//
//	   attr2 := map[string]interface{}{
//		   "CustomIntField": 2,
//		   "CustomKeywordField": "seattle",
//	   }
//	   workflow.UpsertSearchAttributes(ctx, attr2)
//   }
// will eventually have search attributes:
//   map[string]interface{}{
//   	"CustomIntField": 2,
//   	"CustomBoolField": true,
//   	"CustomKeywordField": "seattle",
//   }
// This is only supported when using ElasticSearch.
func UpsertSearchAttributes(ctx Context, attributes map[string]interface{}) error {
	if _, ok := attributes[CadenceChangeVersion]; ok {
		return errors.New("CadenceChangeVersion is a reserved key that cannot be set, please use other key")
	}
	return getWorkflowEnvironment(ctx).UpsertSearchAttributes(attributes)
}

// WithChildWorkflowOptions adds all workflow options to the context.
// The current timeout resolution implementation is in seconds and uses math.Ceil(d.Seconds()) as the duration. But is
// subjected to change in the future.
func WithChildWorkflowOptions(ctx Context, cwo ChildWorkflowOptions) Context {
	ctx1 := setWorkflowEnvOptionsIfNotExist(ctx)
	wfOptions := getWorkflowEnvOptions(ctx1)
	wfOptions.domain = common.StringPtr(cwo.Domain)
	wfOptions.taskListName = common.StringPtr(cwo.TaskList)
	wfOptions.workflowID = cwo.WorkflowID
	wfOptions.executionStartToCloseTimeoutSeconds = common.Int32Ptr(common.Int32Ceil(cwo.ExecutionStartToCloseTimeout.Seconds()))
	wfOptions.taskStartToCloseTimeoutSeconds = common.Int32Ptr(common.Int32Ceil(cwo.TaskStartToCloseTimeout.Seconds()))
	wfOptions.waitForCancellation = cwo.WaitForCancellation
	wfOptions.workflowIDReusePolicy = cwo.WorkflowIDReusePolicy
	wfOptions.retryPolicy = convertRetryPolicy(cwo.RetryPolicy)
	wfOptions.cronSchedule = cwo.CronSchedule
	wfOptions.memo = cwo.Memo
	wfOptions.searchAttributes = cwo.SearchAttributes
	wfOptions.parentClosePolicy = cwo.ParentClosePolicy

	return ctx1
}

// WithWorkflowDomain adds a domain to the context.
func WithWorkflowDomain(ctx Context, name string) Context {
	ctx1 := setWorkflowEnvOptionsIfNotExist(ctx)
	getWorkflowEnvOptions(ctx1).domain = common.StringPtr(name)
	return ctx1
}

// WithWorkflowTaskList adds a task list to the context.
func WithWorkflowTaskList(ctx Context, name string) Context {
	ctx1 := setWorkflowEnvOptionsIfNotExist(ctx)
	getWorkflowEnvOptions(ctx1).taskListName = common.StringPtr(name)
	return ctx1
}

// WithWorkflowID adds a workflowID to the context.
func WithWorkflowID(ctx Context, workflowID string) Context {
	ctx1 := setWorkflowEnvOptionsIfNotExist(ctx)
	getWorkflowEnvOptions(ctx1).workflowID = workflowID
	return ctx1
}

// WithExecutionStartToCloseTimeout adds a workflow execution timeout to the context.
// The current timeout resolution implementation is in seconds and uses math.Ceil(d.Seconds()) as the duration. But is
// subjected to change in the future.
func WithExecutionStartToCloseTimeout(ctx Context, d time.Duration) Context {
	ctx1 := setWorkflowEnvOptionsIfNotExist(ctx)
	getWorkflowEnvOptions(ctx1).executionStartToCloseTimeoutSeconds = common.Int32Ptr(common.Int32Ceil(d.Seconds()))
	return ctx1
}

// WithWorkflowTaskStartToCloseTimeout adds a decision timeout to the context.
// The current timeout resolution implementation is in seconds and uses math.Ceil(d.Seconds()) as the duration. But is
// subjected to change in the future.
func WithWorkflowTaskStartToCloseTimeout(ctx Context, d time.Duration) Context {
	ctx1 := setWorkflowEnvOptionsIfNotExist(ctx)
	getWorkflowEnvOptions(ctx1).taskStartToCloseTimeoutSeconds = common.Int32Ptr(common.Int32Ceil(d.Seconds()))
	return ctx1
}

// WithDataConverter adds DataConverter to the context.
func WithDataConverter(ctx Context, dc DataConverter) Context {
	if dc == nil {
		panic("data converter is nil for WithDataConverter")
	}
	ctx1 := setWorkflowEnvOptionsIfNotExist(ctx)
	getWorkflowEnvOptions(ctx1).dataConverter = dc
	return ctx1
}

// withContextPropagators adds ContextPropagators to the context.
func withContextPropagators(ctx Context, contextPropagators []ContextPropagator) Context {
	ctx1 := setWorkflowEnvOptionsIfNotExist(ctx)
	getWorkflowEnvOptions(ctx1).contextPropagators = contextPropagators
	return ctx1
}

// GetSignalChannel returns channel corresponding to the signal name.
func GetSignalChannel(ctx Context, signalName string) Channel {
	return getWorkflowEnvOptions(ctx).getSignalChannel(ctx, signalName)
}

func newEncodedValue(value []byte, dc DataConverter) Value {
	if dc == nil {
		dc = getDefaultDataConverter()
	}
	return &EncodedValue{value, dc}
}

// Get extract data from encoded data to desired value type. valuePtr is pointer to the actual value type.
func (b EncodedValue) Get(valuePtr interface{}) error {
	if !b.HasValue() {
		return ErrNoData
	}
	return decodeArg(b.dataConverter, b.value, valuePtr)
}

// HasValue return whether there is value
func (b EncodedValue) HasValue() bool {
	return b.value != nil
}

// SideEffect executes the provided function once, records its result into the workflow history. The recorded result on
// history will be returned without executing the provided function during replay. This guarantees the deterministic
// requirement for workflow as the exact same result will be returned in replay.
// Common use case is to run some short non-deterministic code in workflow, like getting random number or new UUID.
// The only way to fail SideEffect is to panic which causes decision task failure. The decision task after timeout is
// rescheduled and re-executed giving SideEffect another chance to succeed.
//
// Caution: do not use SideEffect to modify closures. Always retrieve result from SideEffect's encoded return value.
// For example this code is BROKEN:
//  // Bad example:
//  var random int
//  workflow.SideEffect(func(ctx workflow.Context) interface{} {
//         random = rand.Intn(100)
//         return nil
//  })
//  // random will always be 0 in replay, thus this code is non-deterministic
//  if random < 50 {
//         ....
//  } else {
//         ....
//  }
// On replay the provided function is not executed, the random will always be 0, and the workflow could takes a
// different path breaking the determinism.
//
// Here is the correct way to use SideEffect:
//  // Good example:
//  encodedRandom := SideEffect(func(ctx workflow.Context) interface{} {
//        return rand.Intn(100)
//  })
//  var random int
//  encodedRandom.Get(&random)
//  if random < 50 {
//         ....
//  } else {
//         ....
//  }
func SideEffect(ctx Context, f func(ctx Context) interface{}) Value {
	dc := getDataConverterFromWorkflowContext(ctx)
	future, settable := NewFuture(ctx)
	wrapperFunc := func() ([]byte, error) {
		r := f(ctx)
		return encodeArg(dc, r)
	}
	resultCallback := func(result []byte, err error) {
		settable.Set(EncodedValue{result, dc}, err)
	}
	getWorkflowEnvironment(ctx).SideEffect(wrapperFunc, resultCallback)
	var encoded EncodedValue
	if err := future.Get(ctx, &encoded); err != nil {
		panic(err)
	}
	return encoded
}

// MutableSideEffect executes the provided function once, then it looks up the history for the value with the given id.
// If there is no existing value, then it records the function result as a value with the given id on history;
// otherwise, it compares whether the existing value from history has changed from the new function result by calling the
// provided equals function. If they are equal, it returns the value without recording a new one in history;
//   otherwise, it records the new value with the same id on history.
//
// Caution: do not use MutableSideEffect to modify closures. Always retrieve result from MutableSideEffect's encoded
// return value.
//
// The difference between MutableSideEffect() and SideEffect() is that every new SideEffect() call in non-replay will
// result in a new marker being recorded on history. However, MutableSideEffect() only records a new marker if the value
// changed. During replay, MutableSideEffect() will not execute the function again, but it will return the exact same
// value as it was returning during the non-replay run.
//
// One good use case of MutableSideEffect() is to access dynamically changing config without breaking determinism.
func MutableSideEffect(ctx Context, id string, f func(ctx Context) interface{}, equals func(a, b interface{}) bool) Value {
	wrapperFunc := func() interface{} {
		return f(ctx)
	}
	return getWorkflowEnvironment(ctx).MutableSideEffect(id, wrapperFunc, equals)
}

// DefaultVersion is a version returned by GetVersion for code that wasn't versioned before
const DefaultVersion Version = -1

// CadenceChangeVersion is used as search attributes key to find workflows with specific change version.
const CadenceChangeVersion = "CadenceChangeVersion"

// GetVersion is used to safely perform backwards incompatible changes to workflow definitions.
// It is not allowed to update workflow code while there are workflows running as it is going to break
// determinism. The solution is to have both old code that is used to replay existing workflows
// as well as the new one that is used when it is executed for the first time.
// GetVersion returns maxSupported version when is executed for the first time. This version is recorded into the
// workflow history as a marker event. Even if maxSupported version is changed the version that was recorded is
// returned on replay. DefaultVersion constant contains version of code that wasn't versioned before.
// For example initially workflow has the following code:
//  err = workflow.ExecuteActivity(ctx, foo).Get(ctx, nil)
// it should be updated to
//  err = workflow.ExecuteActivity(ctx, bar).Get(ctx, nil)
// The backwards compatible way to execute the update is
//  v :=  GetVersion(ctx, "fooChange", DefaultVersion, 1)
//  if v  == DefaultVersion {
//      err = workflow.ExecuteActivity(ctx, foo).Get(ctx, nil)
//  } else {
//      err = workflow.ExecuteActivity(ctx, bar).Get(ctx, nil)
//  }
//
// Then bar has to be changed to baz:
//  v :=  GetVersion(ctx, "fooChange", DefaultVersion, 2)
//  if v  == DefaultVersion {
//      err = workflow.ExecuteActivity(ctx, foo).Get(ctx, nil)
//  } else if v == 1 {
//      err = workflow.ExecuteActivity(ctx, bar).Get(ctx, nil)
//  } else {
//      err = workflow.ExecuteActivity(ctx, baz).Get(ctx, nil)
//  }
//
// Later when there are no workflow executions running DefaultVersion the correspondent branch can be removed:
//  v :=  GetVersion(ctx, "fooChange", 1, 2)
//  if v == 1 {
//      err = workflow.ExecuteActivity(ctx, bar).Get(ctx, nil)
//  } else {
//      err = workflow.ExecuteActivity(ctx, baz).Get(ctx, nil)
//  }
//
// It is recommended to keep the GetVersion() call even if single branch is left:
//  GetVersion(ctx, "fooChange", 2, 2)
//  err = workflow.ExecuteActivity(ctx, baz).Get(ctx, nil)
//
// The reason to keep it is: 1) it ensures that if there is older version execution still running, it will fail here
// and not proceed; 2) if you ever need to make more changes for “fooChange”, for example change activity from baz to qux,
// you just need to update the maxVersion from 2 to 3.
//
// Note that, you only need to preserve the first call to GetVersion() for each changeID. All subsequent call to GetVersion()
// with same changeID are safe to remove. However, if you really want to get rid of the first GetVersion() call as well,
// you can do so, but you need to make sure: 1) all older version executions are completed; 2) you can no longer use “fooChange”
// as changeID. If you ever need to make changes to that same part like change from baz to qux, you would need to use a
// different changeID like “fooChange-fix2”, and start minVersion from DefaultVersion again. The code would looks like:
//
//  v := workflow.GetVersion(ctx, "fooChange-fix2", workflow.DefaultVersion, 1)
//  if v == workflow.DefaultVersion {
//    err = workflow.ExecuteActivity(ctx, baz, data).Get(ctx, nil)
//  } else {
//    err = workflow.ExecuteActivity(ctx, qux, data).Get(ctx, nil)
//  }
func GetVersion(ctx Context, changeID string, minSupported, maxSupported Version) Version {
	return getWorkflowEnvironment(ctx).GetVersion(changeID, minSupported, maxSupported)
}

// SetQueryHandler sets the query handler to handle workflow query. The queryType specify which query type this handler
// should handle. The handler must be a function that returns 2 values. The first return value must be a serializable
// result. The second return value must be an error. The handler function could receive any number of input parameters.
// All the input parameter must be serializable. You should call workflow.SetQueryHandler() at the beginning of the workflow
// code. When client calls Client.QueryWorkflow() to cadence server, a task will be generated on server that will be dispatched
// to a workflow worker, which will replay the history events and then execute a query handler based on the query type.
// The query handler will be invoked out of the context of the workflow, meaning that the handler code must not use cadence
// context to do things like workflow.NewChannel(), workflow.Go() or to call any workflow blocking functions like
// Channel.Get() or Future.Get(). Trying to do so in query handler code will fail the query and client will receive
// QueryFailedError.
// Example of workflow code that support query type "current_state":
//  func MyWorkflow(ctx workflow.Context, input string) error {
//    currentState := "started" // this could be any serializable struct
//    err := workflow.SetQueryHandler(ctx, "current_state", func() (string, error) {
//      return currentState, nil
//    })
//    if err != nil {
//      currentState = "failed to register query handler"
//      return err
//    }
//    // your normal workflow code begins here, and you update the currentState as the code makes progress.
//    currentState = "waiting timer"
//    err = NewTimer(ctx, time.Hour).Get(ctx, nil)
//    if err != nil {
//      currentState = "timer failed"
//      return err
//    }
//
//    currentState = "waiting activity"
//    ctx = WithActivityOptions(ctx, myActivityOptions)
//    err = ExecuteActivity(ctx, MyActivity, "my_input").Get(ctx, nil)
//    if err != nil {
//      currentState = "activity failed"
//      return err
//    }
//    currentState = "done"
//    return nil
//  }
func SetQueryHandler(ctx Context, queryType string, handler interface{}) error {
	if strings.HasPrefix(queryType, "__") {
		return errors.New("queryType starts with '__' is reserved for internal use")
	}
	return setQueryHandler(ctx, queryType, handler)
}

// IsReplaying returns whether the current workflow code is replaying.
//
// Warning! Never make decisions, like schedule activity/childWorkflow/timer or send/wait on future/channel, based on
// this flag as it is going to break workflow determinism requirement.
// The only reasonable use case for this flag is to avoid some external actions during replay, like custom logging or
// metric reporting. Please note that Cadence already provide standard logging/metric via workflow.GetLogger(ctx) and
// workflow.GetMetricsScope(ctx), and those standard mechanism are replay-aware and it will automatically suppress during
// replay. Only use this flag if you need custom logging/metrics reporting, for example if you want to log to kafka.
//
// Warning! Any action protected by this flag should not fail or if it does fail should ignore that failure or panic
// on the failure. If workflow don't want to be blocked on those failure, it should ignore those failure; if workflow do
// want to make sure it proceed only when that action succeed then it should panic on that failure. Panic raised from a
// workflow causes decision task to fail and cadence server will rescheduled later to retry.
func IsReplaying(ctx Context) bool {
	return getWorkflowEnvironment(ctx).IsReplaying()
}

// HasLastCompletionResult checks if there is completion result from previous runs.
// This is used in combination with cron schedule. A workflow can be started with an optional cron schedule.
// If a cron workflow wants to pass some data to next schedule, it can return any data and that data will become
// available when next run starts.
// This HasLastCompletionResult() checks if there is such data available passing down from previous successful run.
func HasLastCompletionResult(ctx Context) bool {
	info := GetWorkflowInfo(ctx)
	return len(info.lastCompletionResult) > 0
}

// GetLastCompletionResult extract last completion result from previous run for this cron workflow.
// This is used in combination with cron schedule. A workflow can be started with an optional cron schedule.
// If a cron workflow wants to pass some data to next schedule, it can return any data and that data will become
// available when next run starts.
// This GetLastCompletionResult() extract the data into expected data structure.
func GetLastCompletionResult(ctx Context, d ...interface{}) error {
	info := GetWorkflowInfo(ctx)
	if len(info.lastCompletionResult) == 0 {
		return ErrNoData
	}

	encodedVal := newEncodedValues(info.lastCompletionResult, getDataConverterFromWorkflowContext(ctx))
	return encodedVal.Get(d...)
}
