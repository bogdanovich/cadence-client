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

// All code in this file is private to the package.

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/opentracing/opentracing-go"
	"github.com/pborman/uuid"
	"github.com/uber-go/tally"
	"go.uber.org/cadence/.gen/go/cadence/workflowserviceclient"
	"go.uber.org/cadence/.gen/go/shared"
	"go.uber.org/cadence/internal/common"
	"go.uber.org/cadence/internal/common/backoff"
	"go.uber.org/cadence/internal/common/metrics"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	// Set to 2 pollers for now, can adjust later if needed. The typical RTT (round-trip time) is below 1ms within data
	// center. And the poll API latency is about 5ms. With 2 poller, we could achieve around 300~400 RPS.
	defaultConcurrentPollRoutineSize = 2

	defaultMaxConcurrentActivityExecutionSize = 1000   // Large concurrent activity execution size (1k)
	defaultWorkerActivitiesPerSecond          = 100000 // Large activity executions/sec (unlimited)

	defaultMaxConcurrentLocalActivityExecutionSize = 1000   // Large concurrent activity execution size (1k)
	defaultWorkerLocalActivitiesPerSecond          = 100000 // Large activity executions/sec (unlimited)

	defaultTaskListActivitiesPerSecond = 100000.0 // Large activity executions/sec (unlimited)

	defaultMaxConcurrentTaskExecutionSize = 1000   // hardcoded max task execution size.
	defaultWorkerTaskExecutionRate        = 100000 // Large task execution rate (unlimited)

	defaultPollerRate = 1000

	defaultMaxConcurrentSessionExecutionSize = 1000 // Large concurrent session execution size (1k)

	testTagsContextKey = "cadence-testTags"
)

// Assert that structs do indeed implement the interfaces
var _ Worker = (*aggregatedWorker)(nil)

type (
	// WorkflowWorker wraps the code for hosting workflow types.
	// And worker is mapped 1:1 with task list. If the user want's to poll multiple
	// task list names they might have to manage 'n' workers for 'n' task lists.
	workflowWorker struct {
		executionParameters workerExecutionParameters
		workflowService     workflowserviceclient.Interface
		domain              string
		poller              taskPoller // taskPoller to poll and process the tasks.
		worker              *baseWorker
		localActivityWorker *baseWorker
		identity            string
		stopC               chan struct{}
	}

	// ActivityWorker wraps the code for hosting activity types.
	// TODO: Worker doing heartbeating automatically while activity task is running
	activityWorker struct {
		executionParameters workerExecutionParameters
		workflowService     workflowserviceclient.Interface
		domain              string
		poller              taskPoller
		worker              *baseWorker
		identity            string
		stopC               chan struct{}
	}

	// sessionWorker wraps the code for hosting session creation, completion and
	// activities within a session. The creationWorker polls from a global tasklist,
	// while the activityWorker polls from a resource specific tasklist.
	sessionWorker struct {
		creationWorker Worker
		activityWorker Worker
	}

	// Worker overrides.
	workerOverrides struct {
		workflowTaskHandler WorkflowTaskHandler
		activityTaskHandler ActivityTaskHandler
	}

	// workerExecutionParameters defines worker configure/execution options.
	workerExecutionParameters struct {
		// Task list name to poll.
		TaskList string

		// Defines how many concurrent poll requests for the task list by this worker.
		ConcurrentPollRoutineSize int

		// Defines how many concurrent activity executions by this worker.
		ConcurrentActivityExecutionSize int

		// Defines rate limiting on number of activity tasks that can be executed per second per worker.
		WorkerActivitiesPerSecond float64

		// Defines how many concurrent decision task executions by this worker.
		ConcurrentDecisionTaskExecutionSize int

		// Defines rate limiting on number of decision tasks that can be executed per second per worker.
		WorkerDecisionTasksPerSecond float64

		// Defines how many concurrent local activity executions by this worker.
		ConcurrentLocalActivityExecutionSize int

		// Defines rate limiting on number of local activities that can be executed per second per worker.
		WorkerLocalActivitiesPerSecond float64

		// TaskListActivitiesPerSecond is the throttling limit for activity tasks controlled by the server
		TaskListActivitiesPerSecond float64

		// User can provide an identity for the debuggability. If not provided the framework has
		// a default option.
		Identity string

		MetricsScope tally.Scope

		Logger *zap.Logger

		// Enable logging in replay mode
		EnableLoggingInReplay bool

		// Context to store user provided key/value pairs
		UserContext context.Context

		// Context cancel function to cancel user context
		UserContextCancel context.CancelFunc

		// Disable sticky execution
		DisableStickyExecution bool

		StickyScheduleToStartTimeout time.Duration

		// NonDeterministicWorkflowPolicy is used for configuring how client's decision task handler deals with
		// mismatched history events (presumably arising from non-deterministic workflow definitions).
		NonDeterministicWorkflowPolicy NonDeterministicWorkflowPolicy

		DataConverter DataConverter

		// WorkerStopTimeout is the time delay before hard terminate worker
		WorkerStopTimeout time.Duration

		// WorkerStopChannel is a read only channel listen on worker close. The worker will close the channel before exit.
		WorkerStopChannel <-chan struct{}

		// SessionResourceID is a unique identifier of the resource the session will consume
		SessionResourceID string

		ContextPropagators []ContextPropagator

		Tracer opentracing.Tracer
	}
)

// newWorkflowWorker returns an instance of the workflow worker.
func newWorkflowWorker(
	service workflowserviceclient.Interface,
	domain string,
	params workerExecutionParameters,
	ppMgr pressurePointMgr,
	hostEnv *hostEnvImpl,
) Worker {
	return newWorkflowWorkerInternal(service, domain, params, ppMgr, nil, hostEnv)
}

func ensureRequiredParams(params *workerExecutionParameters) {
	if params.Identity == "" {
		params.Identity = getWorkerIdentity(params.TaskList)
	}
	if params.Logger == nil {
		// create default logger if user does not supply one.
		config := zap.NewProductionConfig()
		// set default time formatter to "2006-01-02T15:04:05.000Z0700"
		config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		//config.Level.SetLevel(zapcore.DebugLevel)
		logger, _ := config.Build()
		params.Logger = logger
		params.Logger.Info("No logger configured for cadence worker. Created default one.")
	}
	if params.MetricsScope == nil {
		params.MetricsScope = tally.NoopScope
		params.Logger.Info("No metrics scope configured for cadence worker. Use NoopScope as default.")
	}
	if params.DataConverter == nil {
		params.DataConverter = getDefaultDataConverter()
		params.Logger.Info("No DataConverter configured for cadence worker. Use default one.")
	}
}

// verifyDomainExist does a DescribeDomain operation on the specified domain with backoff/retry
// It returns an error, if the server returns an EntityNotExist or BadRequest error
// On any other transient error, this method will just return success
func verifyDomainExist(client workflowserviceclient.Interface, domain string, logger *zap.Logger) error {
	ctx := context.Background()
	descDomainOp := func() error {
		tchCtx, cancel, opt := newChannelContext(ctx)
		defer cancel()
		_, err := client.DescribeDomain(tchCtx, &shared.DescribeDomainRequest{Name: &domain}, opt...)
		if err != nil {
			if _, ok := err.(*shared.EntityNotExistsError); ok {
				logger.Error("domain does not exist", zap.String("domain", domain), zap.Error(err))
				return err
			}
			if _, ok := err.(*shared.BadRequestError); ok {
				logger.Error("domain does not exist", zap.String("domain", domain), zap.Error(err))
				return err
			}
			// on any other error, just return true
			logger.Warn("unable to verify if domain exist", zap.String("domain", domain), zap.Error(err))
		}
		return nil
	}

	if len(domain) == 0 {
		return errors.New("domain cannot be empty")
	}

	// exponential backoff retry for upto a minute
	return backoff.Retry(ctx, descDomainOp, createDynamicServiceRetryPolicy(ctx), isServiceTransientError)
}

func newWorkflowWorkerInternal(
	service workflowserviceclient.Interface,
	domain string,
	params workerExecutionParameters,
	ppMgr pressurePointMgr,
	overrides *workerOverrides,
	hostEnv *hostEnvImpl,
) Worker {
	workerStopChannel := make(chan struct{})
	params.WorkerStopChannel = getReadOnlyChannel(workerStopChannel)
	// Get a workflow task handler.
	ensureRequiredParams(&params)
	var taskHandler WorkflowTaskHandler
	if overrides != nil && overrides.workflowTaskHandler != nil {
		taskHandler = overrides.workflowTaskHandler
	} else {
		taskHandler = newWorkflowTaskHandler(domain, params, ppMgr, hostEnv)
	}
	return newWorkflowTaskWorkerInternal(taskHandler, service, domain, params, workerStopChannel)
}

func newWorkflowTaskWorkerInternal(
	taskHandler WorkflowTaskHandler,
	service workflowserviceclient.Interface,
	domain string,
	params workerExecutionParameters,
	stopC chan struct{},
) Worker {
	ensureRequiredParams(&params)
	poller := newWorkflowTaskPoller(
		taskHandler,
		service,
		domain,
		params,
	)
	worker := newBaseWorker(baseWorkerOptions{
		pollerCount:       params.ConcurrentPollRoutineSize,
		pollerRate:        defaultPollerRate,
		maxConcurrentTask: params.ConcurrentDecisionTaskExecutionSize,
		maxTaskPerSecond:  params.WorkerDecisionTasksPerSecond,
		taskWorker:        poller,
		identity:          params.Identity,
		workerType:        "DecisionWorker",
		shutdownTimeout:   params.WorkerStopTimeout},
		params.Logger,
		params.MetricsScope,
		nil,
	)

	// laTunnel is the glue that hookup 3 parts
	laTunnel := &localActivityTunnel{
		taskCh:   make(chan *localActivityTask, 1000),
		resultCh: make(chan interface{}),
	}

	// 1) workflow handler will send local activity task to laTunnel
	if handlerImpl, ok := taskHandler.(*workflowTaskHandlerImpl); ok {
		handlerImpl.laTunnel = laTunnel
	}

	// 2) local activity task poller will poll from laTunnel, and result will be pushed to laTunnel
	localActivityTaskPoller := newLocalActivityPoller(params, laTunnel)
	localActivityWorker := newBaseWorker(baseWorkerOptions{
		pollerCount:       1, // 1 poller (from local channel) is enough for local activity
		maxConcurrentTask: params.ConcurrentLocalActivityExecutionSize,
		maxTaskPerSecond:  params.WorkerLocalActivitiesPerSecond,
		taskWorker:        localActivityTaskPoller,
		identity:          params.Identity,
		workerType:        "LocalActivityWorker",
		shutdownTimeout:   params.WorkerStopTimeout},
		params.Logger,
		params.MetricsScope,
		nil,
	)

	// 3) the result pushed to laTunnel will be send as task to workflow worker to process.
	worker.taskQueueCh = laTunnel.resultCh

	return &workflowWorker{
		executionParameters: params,
		workflowService:     service,
		poller:              poller,
		worker:              worker,
		localActivityWorker: localActivityWorker,
		identity:            params.Identity,
		domain:              domain,
		stopC:               stopC,
	}
}

// Start the worker.
func (ww *workflowWorker) Start() error {
	err := verifyDomainExist(ww.workflowService, ww.domain, ww.worker.logger)
	if err != nil {
		return err
	}
	ww.localActivityWorker.Start()
	ww.worker.Start()
	return nil // TODO: propagate error
}

func (ww *workflowWorker) Run() error {
	err := verifyDomainExist(ww.workflowService, ww.domain, ww.worker.logger)
	if err != nil {
		return err
	}
	ww.localActivityWorker.Start()
	ww.worker.Run()
	return nil
}

// Shutdown the worker.
func (ww *workflowWorker) Stop() {
	close(ww.stopC)
	// TODO: remove the stop methods in favor of the workerStopChannel
	ww.localActivityWorker.Stop()
	ww.worker.Stop()
}

func newSessionWorker(service workflowserviceclient.Interface,
	domain string,
	params workerExecutionParameters,
	overrides *workerOverrides,
	env *hostEnvImpl,
	maxConcurrentSessionExecutionSize int,
) Worker {
	if params.Identity == "" {
		params.Identity = getWorkerIdentity(params.TaskList)
	}
	// For now resourceID is hidden from user so we will always create a unique one for each worker.
	if params.SessionResourceID == "" {
		params.SessionResourceID = uuid.New()
	}
	sessionEnvironment := newSessionEnvironment(params.SessionResourceID, maxConcurrentSessionExecutionSize)

	creationTasklist := getCreationTasklist(params.TaskList)
	params.UserContext = context.WithValue(params.UserContext, sessionEnvironmentContextKey, sessionEnvironment)
	params.TaskList = sessionEnvironment.GetResourceSpecificTasklist()
	activityWorker := newActivityWorker(service, domain, params, overrides, env, nil)

	params.ConcurrentPollRoutineSize = 1
	params.TaskList = creationTasklist
	creationWorker := newActivityWorker(service, domain, params, overrides, env, sessionEnvironment.GetTokenBucket())

	return &sessionWorker{
		creationWorker: creationWorker,
		activityWorker: activityWorker,
	}
}

func (sw *sessionWorker) Start() error {
	err := sw.creationWorker.Start()
	if err != nil {
		return err
	}

	err = sw.activityWorker.Start()
	if err != nil {
		sw.creationWorker.Stop()
		return err
	}
	return nil
}

func (sw *sessionWorker) Run() error {
	err := sw.creationWorker.Start()
	if err != nil {
		return err
	}
	return sw.activityWorker.Run()
}

func (sw *sessionWorker) Stop() {
	sw.creationWorker.Stop()
	sw.activityWorker.Stop()
}

func newActivityWorker(
	service workflowserviceclient.Interface,
	domain string,
	params workerExecutionParameters,
	overrides *workerOverrides,
	env *hostEnvImpl,
	sessionTokenBucket *sessionTokenBucket,
) Worker {
	workerStopChannel := make(chan struct{}, 1)
	params.WorkerStopChannel = getReadOnlyChannel(workerStopChannel)
	ensureRequiredParams(&params)

	// Get a activity task handler.
	var taskHandler ActivityTaskHandler
	if overrides != nil && overrides.activityTaskHandler != nil {
		taskHandler = overrides.activityTaskHandler
	} else {
		taskHandler = newActivityTaskHandler(service, params, env)
	}
	return newActivityTaskWorker(taskHandler, service, domain, params, sessionTokenBucket, workerStopChannel)
}

func newActivityTaskWorker(
	taskHandler ActivityTaskHandler,
	service workflowserviceclient.Interface,
	domain string,
	workerParams workerExecutionParameters,
	sessionTokenBucket *sessionTokenBucket,
	stopC chan struct{},
) (worker Worker) {
	ensureRequiredParams(&workerParams)

	poller := newActivityTaskPoller(
		taskHandler,
		service,
		domain,
		workerParams,
	)

	base := newBaseWorker(
		baseWorkerOptions{
			pollerCount:       workerParams.ConcurrentPollRoutineSize,
			pollerRate:        defaultPollerRate,
			maxConcurrentTask: workerParams.ConcurrentActivityExecutionSize,
			maxTaskPerSecond:  workerParams.WorkerActivitiesPerSecond,
			taskWorker:        poller,
			identity:          workerParams.Identity,
			workerType:        "ActivityWorker",
			shutdownTimeout:   workerParams.WorkerStopTimeout,
			userContextCancel: workerParams.UserContextCancel},
		workerParams.Logger,
		workerParams.MetricsScope,
		sessionTokenBucket,
	)

	return &activityWorker{
		executionParameters: workerParams,
		workflowService:     service,
		worker:              base,
		poller:              poller,
		identity:            workerParams.Identity,
		domain:              domain,
		stopC:               stopC,
	}
}

// Start the worker.
func (aw *activityWorker) Start() error {
	err := verifyDomainExist(aw.workflowService, aw.domain, aw.worker.logger)
	if err != nil {
		return err
	}
	aw.worker.Start()
	return nil // TODO: propagate errors
}

// Run the worker.
func (aw *activityWorker) Run() error {
	err := verifyDomainExist(aw.workflowService, aw.domain, aw.worker.logger)
	if err != nil {
		return err
	}
	aw.worker.Run()
	return nil
}

// Shutdown the worker.
func (aw *activityWorker) Stop() {
	close(aw.stopC)
	aw.worker.Stop()
}

// hostEnvImpl is the implementation of hostEnv
type hostEnvImpl struct {
	sync.Mutex
	workflowFuncMap  map[string]interface{}
	workflowAliasMap map[string]string
	activityFuncMap  map[string]activity
	activityAliasMap map[string]string
}

func (th *hostEnvImpl) RegisterWorkflow(af interface{}) error {
	return th.RegisterWorkflowWithOptions(af, RegisterWorkflowOptions{})
}

func (th *hostEnvImpl) RegisterWorkflowWithOptions(
	af interface{},
	options RegisterWorkflowOptions,
) error {
	// Validate that it is a function
	fnType := reflect.TypeOf(af)
	if err := validateFnFormat(fnType, true); err != nil {
		return err
	}
	fnName := getFunctionName(af)
	alias := options.Name
	registerName := fnName
	if len(alias) > 0 {
		registerName = alias
	}
	// Check if already registered
	if _, ok := th.getWorkflowFn(registerName); ok {
		return fmt.Errorf("workflow name \"%v\" is already registered", registerName)
	}
	th.addWorkflowFn(registerName, af)
	if len(alias) > 0 {
		th.addWorkflowAlias(fnName, alias)
	}
	return nil
}

func (th *hostEnvImpl) RegisterActivity(af interface{}) error {
	return th.RegisterActivityWithOptions(af, RegisterActivityOptions{})
}

func (th *hostEnvImpl) RegisterActivityWithOptions(
	af interface{},
	options RegisterActivityOptions,
) error {
	// Validate that it is a function
	fnType := reflect.TypeOf(af)
	if err := validateFnFormat(fnType, false); err != nil {
		return err
	}
	fnName := getFunctionName(af)
	alias := options.Name
	registerName := fnName
	if len(alias) > 0 {
		registerName = alias
	}
	// Check if already registered
	if _, ok := th.getActivityFn(registerName); ok {
		return fmt.Errorf("activity type \"%v\" is already registered", registerName)
	}
	th.addActivityFn(registerName, af)
	if len(alias) > 0 {
		th.addActivityAlias(fnName, alias)
	}
	return nil
}

func (th *hostEnvImpl) addWorkflowAlias(fnName string, alias string) {
	th.Lock()
	defer th.Unlock()
	th.workflowAliasMap[fnName] = alias
}

func (th *hostEnvImpl) getWorkflowAlias(fnName string) (string, bool) {
	th.Lock()
	defer th.Unlock()
	alias, ok := th.workflowAliasMap[fnName]
	return alias, ok
}

func (th *hostEnvImpl) addWorkflowFn(fnName string, wf interface{}) {
	th.Lock()
	defer th.Unlock()
	th.workflowFuncMap[fnName] = wf
}

func (th *hostEnvImpl) getWorkflowFn(fnName string) (interface{}, bool) {
	th.Lock()
	defer th.Unlock()
	fn, ok := th.workflowFuncMap[fnName]
	return fn, ok
}

func (th *hostEnvImpl) getRegisteredWorkflowTypes() []string {
	th.Lock()
	defer th.Unlock()
	var r []string
	for t := range th.workflowFuncMap {
		r = append(r, t)
	}
	return r
}

func (th *hostEnvImpl) addActivityAlias(fnName string, alias string) {
	th.Lock()
	defer th.Unlock()
	th.activityAliasMap[fnName] = alias
}

func (th *hostEnvImpl) getActivityAlias(fnName string) (string, bool) {
	th.Lock()
	defer th.Unlock()
	alias, ok := th.activityAliasMap[fnName]
	return alias, ok
}

func (th *hostEnvImpl) addActivity(fnName string, a activity) {
	th.Lock()
	defer th.Unlock()
	th.activityFuncMap[fnName] = a
}

func (th *hostEnvImpl) addActivityFn(fnName string, af interface{}) {
	th.addActivity(fnName, &activityExecutor{fnName, af})
}

func (th *hostEnvImpl) getActivity(fnName string) (activity, bool) {
	th.Lock()
	defer th.Unlock()
	a, ok := th.activityFuncMap[fnName]
	return a, ok
}

func (th *hostEnvImpl) getActivityFn(fnName string) (interface{}, bool) {
	if a, ok := th.getActivity(fnName); ok {
		return a.GetFunction(), ok
	}
	return nil, false
}

func (th *hostEnvImpl) getRegisteredActivities() []activity {
	activities := make([]activity, 0, len(th.activityFuncMap))
	for _, a := range th.activityFuncMap {
		activities = append(activities, a)
	}
	return activities
}

func isUseThriftEncoding(objs []interface{}) bool {
	// NOTE: our criteria to use which encoder is simple if all the types are serializable using thrift then we use
	// thrift encoder. For everything else we default to gob.

	if len(objs) == 0 {
		return false
	}

	for i := 0; i < len(objs); i++ {
		if !isThriftType(objs[i]) {
			return false
		}
	}
	return true
}

func isUseThriftDecoding(objs []interface{}) bool {
	// NOTE: our criteria to use which encoder is simple if all the types are de-serializable using thrift then we use
	// thrift decoder. For everything else we default to gob.

	if len(objs) == 0 {
		return false
	}

	for i := 0; i < len(objs); i++ {
		rVal := reflect.ValueOf(objs[i])
		if rVal.Kind() != reflect.Ptr || !isThriftType(reflect.Indirect(rVal).Interface()) {
			return false
		}
	}
	return true
}

func (th *hostEnvImpl) getWorkflowDefinition(wt WorkflowType) (workflowDefinition, error) {
	lookup := wt.Name
	if alias, ok := th.getWorkflowAlias(lookup); ok {
		lookup = alias
	}
	wf, ok := th.getWorkflowFn(lookup)
	if !ok {
		supported := strings.Join(th.getRegisteredWorkflowTypes(), ", ")
		return nil, fmt.Errorf("unable to find workflow type: %v. Supported types: [%v]", lookup, supported)
	}
	wd := &workflowExecutor{name: lookup, fn: wf}
	return newWorkflowDefinition(wd), nil
}

// Validate function parameters.
func validateFnFormat(fnType reflect.Type, isWorkflow bool) error {
	if fnType.Kind() != reflect.Func {
		return fmt.Errorf("expected a func as input but was %s", fnType.Kind())
	}
	if isWorkflow {
		if fnType.NumIn() < 1 {
			return fmt.Errorf(
				"expected at least one argument of type workflow.Context in function, found %d input arguments",
				fnType.NumIn(),
			)
		}
		if !isWorkflowContext(fnType.In(0)) {
			return fmt.Errorf("expected first argument to be workflow.Context but found %s", fnType.In(0))
		}
	}

	// Return values
	// We expect either
	// 	<result>, error
	//	(or) just error
	if fnType.NumOut() < 1 || fnType.NumOut() > 2 {
		return fmt.Errorf(
			"expected function to return result, error or just error, but found %d return values", fnType.NumOut(),
		)
	}
	if fnType.NumOut() > 1 && !isValidResultType(fnType.Out(0)) {
		return fmt.Errorf(
			"expected function first return value to return valid type but found: %v", fnType.Out(0).Kind(),
		)
	}
	if !isError(fnType.Out(fnType.NumOut() - 1)) {
		return fmt.Errorf(
			"expected function second return value to return error but found %v", fnType.Out(fnType.NumOut()-1).Kind(),
		)
	}
	return nil
}

// encode multiple arguments(arguments to a function).
func encodeArgs(dc DataConverter, args []interface{}) ([]byte, error) {
	if dc == nil {
		return getDefaultDataConverter().ToData(args...)
	}
	return dc.ToData(args...)
}

// decode multiple arguments(arguments to a function).
func decodeArgs(dc DataConverter, fnType reflect.Type, data []byte) (result []reflect.Value, err error) {
	if dc == nil {
		dc = getDefaultDataConverter()
	}
	var r []interface{}
argsLoop:
	for i := 0; i < fnType.NumIn(); i++ {
		argT := fnType.In(i)
		if i == 0 && (isActivityContext(argT) || isWorkflowContext(argT)) {
			continue argsLoop
		}
		arg := reflect.New(argT).Interface()
		r = append(r, arg)
	}
	err = dc.FromData(data, r...)
	if err != nil {
		return
	}
	for i := 0; i < len(r); i++ {
		result = append(result, reflect.ValueOf(r[i]).Elem())
	}
	return
}

// encode single value(like return parameter).
func encodeArg(dc DataConverter, arg interface{}) ([]byte, error) {
	if dc == nil {
		return getDefaultDataConverter().ToData(arg)
	}
	return dc.ToData(arg)
}

// decode single value(like return parameter).
func decodeArg(dc DataConverter, data []byte, to interface{}) error {
	if dc == nil {
		return getDefaultDataConverter().FromData(data, to)
	}
	return dc.FromData(data, to)
}

func decodeAndAssignValue(dc DataConverter, from interface{}, toValuePtr interface{}) error {
	if toValuePtr == nil {
		return nil
	}
	if rf := reflect.ValueOf(toValuePtr); rf.Type().Kind() != reflect.Ptr {
		return errors.New("value parameter provided is not a pointer")
	}
	if data, ok := from.([]byte); ok {
		if err := decodeArg(dc, data, toValuePtr); err != nil {
			return err
		}
	} else if fv := reflect.ValueOf(from); fv.IsValid() {
		fromType := fv.Type()
		toType := reflect.TypeOf(toValuePtr).Elem()
		assignable := fromType.AssignableTo(toType)
		if !assignable {
			return fmt.Errorf("%s is not assignable to  %s", fromType.Name(), toType.Name())
		}
		reflect.ValueOf(toValuePtr).Elem().Set(fv)
	}
	return nil
}

var typeOfByteSlice = reflect.TypeOf(([]byte)(nil))

func isTypeByteSlice(inType reflect.Type) bool {
	return inType == typeOfByteSlice || inType == reflect.PtrTo(typeOfByteSlice)
}

var once sync.Once

// Singleton to hold the host registration details.
var thImpl *hostEnvImpl

func newHostEnvironment() *hostEnvImpl {
	return &hostEnvImpl{
		workflowFuncMap:  make(map[string]interface{}),
		workflowAliasMap: make(map[string]string),
		activityFuncMap:  make(map[string]activity),
		activityAliasMap: make(map[string]string),
	}
}

func getHostEnvironment() *hostEnvImpl {
	once.Do(func() {
		thImpl = newHostEnvironment()
	})
	return thImpl
}

// Wrapper to execute workflow functions.
type workflowExecutor struct {
	name string
	fn   interface{}
}

func (we *workflowExecutor) Execute(ctx Context, input []byte) ([]byte, error) {
	fnType := reflect.TypeOf(we.fn)
	// Workflow context.
	args := []reflect.Value{reflect.ValueOf(ctx)}

	dataConverter := getWorkflowEnvOptions(ctx).dataConverter
	if fnType.NumIn() > 1 && isTypeByteSlice(fnType.In(1)) {
		// 0 - is workflow context.
		// 1 ... input types.
		args = append(args, reflect.ValueOf(input))
	} else {
		decoded, err := decodeArgs(dataConverter, fnType, input)
		if err != nil {
			return nil, fmt.Errorf(
				"unable to decode the workflow function input bytes with error: %v, function name: %v",
				err, we.name)
		}
		args = append(args, decoded...)
	}

	// Invoke the workflow with arguments.
	fnValue := reflect.ValueOf(we.fn)
	retValues := fnValue.Call(args)
	return validateFunctionAndGetResults(we.fn, retValues, dataConverter)
}

// Wrapper to execute activity functions.
type activityExecutor struct {
	name string
	fn   interface{}
}

func (ae *activityExecutor) ActivityType() ActivityType {
	return ActivityType{Name: ae.name}
}

func (ae *activityExecutor) GetFunction() interface{} {
	return ae.fn
}

func (ae *activityExecutor) Execute(ctx context.Context, input []byte) ([]byte, error) {
	fnType := reflect.TypeOf(ae.fn)
	args := []reflect.Value{}
	dataConverter := getDataConverterFromActivityCtx(ctx)

	// activities optionally might not take context.
	if fnType.NumIn() > 0 && isActivityContext(fnType.In(0)) {
		args = append(args, reflect.ValueOf(ctx))
	}

	if fnType.NumIn() == 1 && isTypeByteSlice(fnType.In(0)) {
		args = append(args, reflect.ValueOf(input))
	} else {
		decoded, err := decodeArgs(dataConverter, fnType, input)
		if err != nil {
			return nil, fmt.Errorf(
				"unable to decode the activity function input bytes with error: %v for function name: %v",
				err, ae.name)
		}
		args = append(args, decoded...)
	}

	fnValue := reflect.ValueOf(ae.fn)
	retValues := fnValue.Call(args)
	return validateFunctionAndGetResults(ae.fn, retValues, dataConverter)
}

func (ae *activityExecutor) ExecuteWithActualArgs(ctx context.Context, actualArgs []interface{}) ([]byte, error) {
	retValues := ae.executeWithActualArgsWithoutParseResult(ctx, actualArgs)
	dataConverter := getDataConverterFromActivityCtx(ctx)

	return validateFunctionAndGetResults(ae.fn, retValues, dataConverter)
}

func (ae *activityExecutor) executeWithActualArgsWithoutParseResult(ctx context.Context, actualArgs []interface{}) []reflect.Value {
	fnType := reflect.TypeOf(ae.fn)
	args := []reflect.Value{}

	// activities optionally might not take context.
	argsOffeset := 0
	if fnType.NumIn() > 0 && isActivityContext(fnType.In(0)) {
		args = append(args, reflect.ValueOf(ctx))
		argsOffeset = 1
	}

	for i, arg := range actualArgs {
		if arg == nil {
			args = append(args, reflect.New(fnType.In(i+argsOffeset)).Elem())
		} else {
			args = append(args, reflect.ValueOf(arg))
		}
	}

	fnValue := reflect.ValueOf(ae.fn)
	retValues := fnValue.Call(args)
	return retValues
}

func getDataConverterFromActivityCtx(ctx context.Context) DataConverter {
	if ctx == nil || ctx.Value(activityEnvContextKey) == nil {
		return getDefaultDataConverter()
	}
	info := ctx.Value(activityEnvContextKey).(*activityEnvironment)
	if info.dataConverter == nil {
		return getDefaultDataConverter()
	}
	return info.dataConverter
}

// aggregatedWorker combines management of both workflowWorker and activityWorker worker lifecycle.
type aggregatedWorker struct {
	workflowWorker Worker
	activityWorker Worker
	sessionWorker  Worker
	logger         *zap.Logger
	hostEnv        *hostEnvImpl
}

func (aw *aggregatedWorker) Start() error {
	if err := initBinaryChecksum(); err != nil {
		return fmt.Errorf("failed to get executable checksum: %v", err)
	}

	if !isInterfaceNil(aw.workflowWorker) {
		if len(aw.hostEnv.getRegisteredWorkflowTypes()) == 0 {
			aw.logger.Warn(
				"Starting worker without any workflows. Workflows must be registered before start.",
			)
		}
		if err := aw.workflowWorker.Start(); err != nil {
			return err
		}
	}
	if !isInterfaceNil(aw.activityWorker) {
		if len(aw.hostEnv.getRegisteredActivities()) == 0 {
			aw.logger.Warn(
				"Starting worker without any activities. Activities must be registered before start.",
			)
		}
		if err := aw.activityWorker.Start(); err != nil {
			// stop workflow worker.
			if !isInterfaceNil(aw.workflowWorker) {
				aw.workflowWorker.Stop()
			}
			return err
		}
	}

	if !isInterfaceNil(aw.sessionWorker) {
		if err := aw.sessionWorker.Start(); err != nil {
			// stop workflow worker and activity worker.
			if !isInterfaceNil(aw.workflowWorker) {
				aw.workflowWorker.Stop()
			}
			if !isInterfaceNil(aw.activityWorker) {
				aw.activityWorker.Stop()
			}
			return err
		}
	}

	aw.logger.Info("Started Worker")
	return nil
}

var binaryChecksum string
var binaryChecksumLock sync.Mutex

func SetBinaryChecksum(checksum string) {
	binaryChecksumLock.Lock()
	defer binaryChecksumLock.Unlock()
	binaryChecksum = checksum
}

func initBinaryChecksum() error {
	binaryChecksumLock.Lock()
	defer binaryChecksumLock.Unlock()

	if len(binaryChecksum) > 0 {
		return nil
	}

	exec, err := os.Executable()
	if err != nil {
		return err
	}

	f, err := os.Open(exec)
	if err != nil {
		return err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}

	checksum := h.Sum(nil)
	binaryChecksum = hex.EncodeToString(checksum[:])

	return nil
}

func getBinaryChecksum() string {
	if len(binaryChecksum) == 0 {
		initBinaryChecksum()
	}

	return binaryChecksum
}

func (aw *aggregatedWorker) Run() error {
	if err := aw.Start(); err != nil {
		return err
	}
	d := <-getKillSignal()
	aw.logger.Info("Worker has been killed", zap.String("Signal", d.String()))
	aw.Stop()
	return nil
}

func (aw *aggregatedWorker) Stop() {
	if !isInterfaceNil(aw.workflowWorker) {
		aw.workflowWorker.Stop()
	}
	if !isInterfaceNil(aw.activityWorker) {
		aw.activityWorker.Stop()
	}
	if !isInterfaceNil(aw.sessionWorker) {
		aw.sessionWorker.Stop()
	}
	aw.logger.Info("Stopped Worker")
}

// aggregatedWorker returns an instance to manage the workers. Use defaultConcurrentPollRoutineSize (which is 2) as
// poller size. The typical RTT (round-trip time) is below 1ms within data center. And the poll API latency is about 5ms.
// With 2 poller, we could achieve around 300~400 RPS.
func newAggregatedWorker(
	service workflowserviceclient.Interface,
	domain string,
	taskList string,
	options WorkerOptions,
) (worker Worker) {
	wOptions := augmentWorkerOptions(options)
	ctx := wOptions.BackgroundActivityContext
	if ctx == nil {
		ctx = context.Background()
	}
	backgroundActivityContext, backgroundActivityContextCancel := context.WithCancel(ctx)

	workerParams := workerExecutionParameters{
		TaskList:                             taskList,
		ConcurrentPollRoutineSize:            defaultConcurrentPollRoutineSize,
		ConcurrentActivityExecutionSize:      wOptions.MaxConcurrentActivityExecutionSize,
		WorkerActivitiesPerSecond:            wOptions.WorkerActivitiesPerSecond,
		ConcurrentLocalActivityExecutionSize: wOptions.MaxConcurrentLocalActivityExecutionSize,
		WorkerLocalActivitiesPerSecond:       wOptions.WorkerLocalActivitiesPerSecond,
		ConcurrentDecisionTaskExecutionSize:  wOptions.MaxConcurrentDecisionTaskExecutionSize,
		WorkerDecisionTasksPerSecond:         wOptions.WorkerDecisionTasksPerSecond,
		Identity:                             wOptions.Identity,
		MetricsScope:                         wOptions.MetricsScope,
		Logger:                               wOptions.Logger,
		EnableLoggingInReplay:                wOptions.EnableLoggingInReplay,
		UserContext:                          backgroundActivityContext,
		UserContextCancel:                    backgroundActivityContextCancel,
		DisableStickyExecution:               wOptions.DisableStickyExecution,
		StickyScheduleToStartTimeout:         wOptions.StickyScheduleToStartTimeout,
		TaskListActivitiesPerSecond:          wOptions.TaskListActivitiesPerSecond,
		NonDeterministicWorkflowPolicy:       wOptions.NonDeterministicWorkflowPolicy,
		DataConverter:                        wOptions.DataConverter,
		WorkerStopTimeout:                    wOptions.WorkerStopTimeout,
		ContextPropagators:                   wOptions.ContextPropagators,
		Tracer:                               wOptions.Tracer,
	}

	ensureRequiredParams(&workerParams)
	workerParams.MetricsScope = tagScope(workerParams.MetricsScope, tagDomain, domain, tagTaskList, taskList, clientImplHeaderName, clientImplHeaderValue)
	workerParams.Logger = workerParams.Logger.With(
		zapcore.Field{Key: tagDomain, Type: zapcore.StringType, String: domain},
		zapcore.Field{Key: tagTaskList, Type: zapcore.StringType, String: taskList},
		zapcore.Field{Key: tagWorkerID, Type: zapcore.StringType, String: workerParams.Identity},
	)
	logger := workerParams.Logger
	service = metrics.NewWorkflowServiceWrapper(service, workerParams.MetricsScope)

	processTestTags(&wOptions, &workerParams)

	hostEnv := getHostEnvironment()
	// workflow factory.
	var workflowWorker Worker
	if !wOptions.DisableWorkflowWorker {
		testTags := getTestTags(wOptions.BackgroundActivityContext)
		if testTags != nil && len(testTags) > 0 {
			workflowWorker = newWorkflowWorkerWithPressurePoints(
				service,
				domain,
				workerParams,
				testTags,
				hostEnv,
			)
		} else {
			workflowWorker = newWorkflowWorker(
				service,
				domain,
				workerParams,
				nil,
				hostEnv,
			)
		}
	}

	// activity types.
	var activityWorker Worker

	if !wOptions.DisableActivityWorker {
		activityWorker = newActivityWorker(
			service,
			domain,
			workerParams,
			nil,
			hostEnv,
			nil,
		)
	}

	var sessionWorker Worker
	if wOptions.EnableSessionWorker {
		sessionWorker = newSessionWorker(
			service,
			domain,
			workerParams,
			nil,
			hostEnv,
			wOptions.MaxConcurrentSessionExecutionSize,
		)
	}

	return &aggregatedWorker{
		workflowWorker: workflowWorker,
		activityWorker: activityWorker,
		sessionWorker:  sessionWorker,
		logger:         logger,
		hostEnv:        hostEnv,
	}
}

// tagScope with one or multiple tags, like
// tagScope(scope, tag1, val1, tag2, val2)
func tagScope(metricsScope tally.Scope, keyValueinPairs ...string) tally.Scope {
	if metricsScope == nil {
		metricsScope = tally.NoopScope
	}
	if len(keyValueinPairs)%2 != 0 {
		panic("tagScope key value are not in pairs")
	}
	tagsMap := map[string]string{}
	for i := 0; i < len(keyValueinPairs); i += 2 {
		tagsMap[keyValueinPairs[i]] = keyValueinPairs[i+1]
	}
	return metricsScope.Tagged(tagsMap)
}

func processTestTags(wOptions *WorkerOptions, ep *workerExecutionParameters) {
	testTags := getTestTags(wOptions.BackgroundActivityContext)
	if testTags != nil {
		if paramsOverride, ok := testTags[workerOptionsConfig]; ok {
			for key, val := range paramsOverride {
				switch key {
				case workerOptionsConfigConcurrentPollRoutineSize:
					if size, err := strconv.Atoi(val); err == nil {
						ep.ConcurrentPollRoutineSize = size
					}
				}
			}
		}
	}
}

func isWorkflowContext(inType reflect.Type) bool {
	// NOTE: We don't expect any one to derive from workflow context.
	return inType == reflect.TypeOf((*Context)(nil)).Elem()
}

func isValidResultType(inType reflect.Type) bool {
	// https://golang.org/pkg/reflect/#Kind
	switch inType.Kind() {
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return false
	}

	return true
}

func isError(inType reflect.Type) bool {
	errorElem := reflect.TypeOf((*error)(nil)).Elem()
	return inType != nil && inType.Implements(errorElem)
}

func getFunctionName(i interface{}) string {
	return runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
}

func isInterfaceNil(i interface{}) bool {
	return i == nil || reflect.ValueOf(i).IsNil()
}

func getReadOnlyChannel(c chan struct{}) <-chan struct{} {
	return c
}

// encoding is capable of encoding and decoding objects
type encoding interface {
	Marshal([]interface{}) ([]byte, error)
	Unmarshal([]byte, []interface{}) error
}

// jsonEncoding encapsulates json encoding and decoding
type jsonEncoding struct {
}

// Marshal encodes an array of object into bytes
func (g jsonEncoding) Marshal(objs []interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i, obj := range objs {
		if err := enc.Encode(obj); err != nil {
			if err == io.EOF {
				return nil, fmt.Errorf("missing argument at index %d of type %T", i, obj)
			}
			return nil, fmt.Errorf(
				"unable to encode argument: %d, %v, with json error: %v", i, reflect.TypeOf(obj), err)
		}
	}
	return buf.Bytes(), nil
}

// Unmarshal decodes a byte array into the passed in objects
func (g jsonEncoding) Unmarshal(data []byte, objs []interface{}) error {
	dec := json.NewDecoder(bytes.NewBuffer(data))
	for i, obj := range objs {
		if err := dec.Decode(obj); err != nil {
			return fmt.Errorf(
				"unable to decode argument: %d, %v, with json error: %v", i, reflect.TypeOf(obj), err)
		}
	}
	return nil
}

func isThriftType(v interface{}) bool {
	// NOTE: Thrift serialization works only if the values are pointers.
	// Thrift has a validation that it meets thift.TStruct which has Read/Write pointer receivers.

	if reflect.ValueOf(v).Kind() != reflect.Ptr {
		return false
	}
	t := reflect.TypeOf((*thrift.TStruct)(nil)).Elem()
	return reflect.TypeOf(v).Implements(t)
}

// thriftEncoding encapsulates thrift serializer/de-serializer.
type thriftEncoding struct{}

// Marshal encodes an array of thrift into bytes
func (g thriftEncoding) Marshal(objs []interface{}) ([]byte, error) {
	tlist := []thrift.TStruct{}
	for i := 0; i < len(objs); i++ {
		if !isThriftType(objs[i]) {
			return nil, fmt.Errorf("pointer to thrift.TStruct type is required for %v argument", i+1)
		}
		t := reflect.ValueOf(objs[i]).Interface().(thrift.TStruct)
		tlist = append(tlist, t)
	}
	return common.TListSerialize(tlist)
}

// Unmarshal decodes an array of thrift into bytes
func (g thriftEncoding) Unmarshal(data []byte, objs []interface{}) error {
	tlist := []thrift.TStruct{}
	for i := 0; i < len(objs); i++ {
		rVal := reflect.ValueOf(objs[i])
		if rVal.Kind() != reflect.Ptr || !isThriftType(reflect.Indirect(rVal).Interface()) {
			return fmt.Errorf("pointer to pointer thrift.TStruct type is required for %v argument", i+1)
		}
		t := reflect.New(rVal.Elem().Type().Elem()).Interface().(thrift.TStruct)
		tlist = append(tlist, t)
	}

	if err := common.TListDeserialize(tlist, data); err != nil {
		return err
	}

	for i := 0; i < len(tlist); i++ {
		reflect.ValueOf(objs[i]).Elem().Set(reflect.ValueOf(tlist[i]))
	}

	return nil
}

func augmentWorkerOptions(options WorkerOptions) WorkerOptions {
	if options.MaxConcurrentActivityExecutionSize == 0 {
		options.MaxConcurrentActivityExecutionSize = defaultMaxConcurrentActivityExecutionSize
	}
	if options.WorkerActivitiesPerSecond == 0 {
		options.WorkerActivitiesPerSecond = defaultWorkerActivitiesPerSecond
	}
	if options.MaxConcurrentDecisionTaskExecutionSize == 0 {
		options.MaxConcurrentDecisionTaskExecutionSize = defaultMaxConcurrentTaskExecutionSize
	}
	if options.WorkerDecisionTasksPerSecond == 0 {
		options.WorkerDecisionTasksPerSecond = defaultWorkerTaskExecutionRate
	}
	if options.MaxConcurrentLocalActivityExecutionSize == 0 {
		options.MaxConcurrentLocalActivityExecutionSize = defaultMaxConcurrentLocalActivityExecutionSize
	}
	if options.WorkerLocalActivitiesPerSecond == 0 {
		options.WorkerLocalActivitiesPerSecond = defaultWorkerLocalActivitiesPerSecond
	}
	if options.TaskListActivitiesPerSecond == 0 {
		options.TaskListActivitiesPerSecond = defaultTaskListActivitiesPerSecond
	}
	if options.StickyScheduleToStartTimeout.Seconds() == 0 {
		options.StickyScheduleToStartTimeout = stickyDecisionScheduleToStartTimeoutSeconds * time.Second
	}
	if options.DataConverter == nil {
		options.DataConverter = getDefaultDataConverter()
	}
	if options.MaxConcurrentSessionExecutionSize == 0 {
		options.MaxConcurrentSessionExecutionSize = defaultMaxConcurrentSessionExecutionSize
	}

	// if the user passes in a tracer then add a tracing context propagator
	if options.Tracer != nil {
		options.ContextPropagators = append(options.ContextPropagators, NewTracingContextPropagator(options.Logger, options.Tracer))
	} else {
		options.Tracer = opentracing.NoopTracer{}
	}
	return options
}

// getTestTags returns the test tags in the context.
func getTestTags(ctx context.Context) map[string]map[string]string {
	if ctx != nil {
		env := ctx.Value(testTagsContextKey)
		if env != nil {
			return env.(map[string]map[string]string)
		}
	}
	return nil
}
