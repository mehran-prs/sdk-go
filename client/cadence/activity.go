package cadence

import (
	"context"
	"time"

	"github.com/uber-go/cadence-client/.gen/go/shared"
	"go.uber.org/zap"
)

type (
	// ActivityType identifies a activity type.
	ActivityType struct {
		Name string
	}

	// ActivityInfo contains information about currently executing activity.
	ActivityInfo struct {
		TaskToken         []byte
		WorkflowExecution WorkflowExecution
		ActivityID        string
		ActivityType      ActivityType
	}
)

// RegisterActivity - register a activity function with the framework.
// A activity takes a context and input and returns a (result, error) or just error.
// Examples:
//	func sampleActivity(ctx context.Context, input []byte) (result []byte, err error)
//	func sampleActivity(ctx context.Context, arg1 int, arg2 string) (result *customerStruct, err error)
//	func sampleActivity(ctx context.Context) (err error)
//	func sampleActivity() (result string, err error)
//	func sampleActivity(arg1 bool) (result int, err error)
//	func sampleActivity(arg1 bool) (err error)
// Serialization of all primitive types, structures is supported ... except channels, functions, variadic, unsafe pointer.
// This method calls panic if activityFunc doesn't comply with the expected format.
func RegisterActivity(activityFunc interface{}) {
	thImpl := getHostEnvironment()
	err := thImpl.RegisterActivity(activityFunc)
	if err != nil {
		panic(err)
	}
}

// GetActivityInfo returns information about currently executing activity.
func GetActivityInfo(ctx context.Context) ActivityInfo {
	env := getActivityEnv(ctx)
	return ActivityInfo{
		ActivityID:        env.activityID,
		ActivityType:      env.activityType,
		TaskToken:         env.taskToken,
		WorkflowExecution: env.workflowExecution,
	}
}

// GetActivityLogger returns a logger that can be used in activity
func GetActivityLogger(ctx context.Context) *zap.Logger {
	env := getActivityEnv(ctx)
	return env.logger
}

// RecordActivityHeartbeat sends heartbeat for the currently executing activity
// If the activity is either cancelled (or) workflow/activity doesn't exist then we would cancel
// the context with error context.Canceled.
// 	TODO: we don't have a way to distinguish between the two cases when context is cancelled because
// 	context doesn't support overriding value of ctx.Error.
// 	TODO: Implement automatic heartbeating with cancellation through ctx.
// details - the details that you provided here can be seen in the worflow when it receives TimeoutError, you
//	can check error TimeOutType()/Details().
func RecordActivityHeartbeat(ctx context.Context, details ...interface{}) {
	data, err := getHostEnvironment().encodeArgs(details)
	if err != nil {
		panic(err)
	}
	env := getActivityEnv(ctx)
	err = env.serviceInvoker.Heartbeat(data)
	if err != nil {
		log := GetActivityLogger(ctx)
		log.Debug("RecordActivityHeartbeat With Error:", zap.Error(err))
	}
}

// ServiceInvoker abstracts calls to the Cadence service from an activity implementation.
// Implement to unit test activities.
type ServiceInvoker interface {
	// Returns ActivityTaskCanceledError if activity is cancelled
	Heartbeat(details []byte) error
}

// WithActivityTask adds activity specific information into context.
// Use this method to unit test activity implementations that use context extractor methodshared.
func WithActivityTask(
	ctx context.Context,
	task *shared.PollForActivityTaskResponse,
	invoker ServiceInvoker,
	logger *zap.Logger,
) context.Context {
	// TODO: Add activity start to close timeout to activity task and use it as the deadline
	return context.WithValue(ctx, activityEnvContextKey, &activityEnvironment{
		taskToken:      task.TaskToken,
		serviceInvoker: invoker,
		activityType:   ActivityType{Name: *task.ActivityType.Name},
		activityID:     *task.ActivityId,
		workflowExecution: WorkflowExecution{
			RunID: *task.WorkflowExecution.RunId,
			ID:    *task.WorkflowExecution.WorkflowId},
		logger: logger,
	})
}

// ActivityOptions stores all activity-specific parameters that will
// be stored inside of a context.
type ActivityOptions interface {
	WithTaskList(name string) ActivityOptions
	WithScheduleToCloseTimeout(d time.Duration) ActivityOptions
	WithScheduleToStartTimeout(d time.Duration) ActivityOptions
	WithStartToCloseTimeout(d time.Duration) ActivityOptions
	WithHeartbeatTimeout(d time.Duration) ActivityOptions
	WithWaitForCancellation(wait bool) ActivityOptions
}

// NewActivityOptions returns an instance of activity options that can be used to specify
// options for an activity through context.
//			ctx1 := WithActivityOptions(ctx, NewActivityOptions().
//					WithTaskList("exampleTaskList").
//					WithScheduleToCloseTimeout(time.Second).
//					WithScheduleToStartTimeout(time.Second).
//					WithHeartbeatTimeout(0)
func NewActivityOptions() ActivityOptions {
	return &activityOptions{}
}

// WithActivityOptions adds all options to the context.
func WithActivityOptions(ctx Context, options ActivityOptions) Context {
	ao := options.(*activityOptions)
	ctx1 := setActivityParametersIfNotExist(ctx)
	eap := getActivityOptions(ctx1)
	if ao.taskListName != nil {
		eap.TaskListName = *ao.taskListName
	}
	if ao.scheduleToCloseTimeoutSeconds != nil {
		eap.ScheduleToCloseTimeoutSeconds = *ao.scheduleToCloseTimeoutSeconds
	}
	if ao.startToCloseTimeoutSeconds != nil {
		eap.StartToCloseTimeoutSeconds = *ao.startToCloseTimeoutSeconds
	}
	if ao.scheduleToStartTimeoutSeconds != nil {
		eap.ScheduleToStartTimeoutSeconds = *ao.scheduleToStartTimeoutSeconds
	}
	if ao.heartbeatTimeoutSeconds != nil {
		eap.HeartbeatTimeoutSeconds = *ao.heartbeatTimeoutSeconds
	}
	if ao.waitForCancellation != nil {
		eap.WaitForCancellation = *ao.waitForCancellation
	}
	if ao.activityID != nil {
		eap.ActivityID = ao.activityID
	}
	return ctx1
}

// WithTaskList adds a task list to the context.
func WithTaskList(ctx Context, name string) Context {
	ctx1 := setActivityParametersIfNotExist(ctx)
	getActivityOptions(ctx1).TaskListName = name
	return ctx1
}

// WithScheduleToCloseTimeout adds a timeout to the context.
func WithScheduleToCloseTimeout(ctx Context, d time.Duration) Context {
	ctx1 := setActivityParametersIfNotExist(ctx)
	getActivityOptions(ctx1).ScheduleToCloseTimeoutSeconds = int32(d.Seconds())
	return ctx1
}

// WithScheduleToStartTimeout adds a timeout to the context.
func WithScheduleToStartTimeout(ctx Context, d time.Duration) Context {
	ctx1 := setActivityParametersIfNotExist(ctx)
	getActivityOptions(ctx1).ScheduleToStartTimeoutSeconds = int32(d.Seconds())
	return ctx1
}

// WithStartToCloseTimeout adds a timeout to the context.
func WithStartToCloseTimeout(ctx Context, d time.Duration) Context {
	ctx1 := setActivityParametersIfNotExist(ctx)
	getActivityOptions(ctx1).StartToCloseTimeoutSeconds = int32(d.Seconds())
	return ctx1
}

// WithHeartbeatTimeout adds a timeout to the context.
func WithHeartbeatTimeout(ctx Context, d time.Duration) Context {
	ctx1 := setActivityParametersIfNotExist(ctx)
	getActivityOptions(ctx1).HeartbeatTimeoutSeconds = int32(d.Seconds())
	return ctx1
}

// WithWaitForCancellation adds wait for the cacellation to the context.
func WithWaitForCancellation(ctx Context, wait bool) Context {
	ctx1 := setActivityParametersIfNotExist(ctx)
	getActivityOptions(ctx1).WaitForCancellation = wait
	return ctx1
}