package cadence

// All code in this file is private to the package.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/uber-go/cadence-client/common"
	"go.uber.org/zap"
)

type (
	// activity is an interface of an activity implementation.
	activity interface {
		Execute(ctx context.Context, input []byte) ([]byte, error)
		ActivityType() ActivityType
	}

	activityInfo struct {
		activityID string
	}

	// executeActivityParameters configuration parameters for scheduling an activity
	executeActivityParameters struct {
		ActivityID                    *string // Users can choose IDs but our framework makes it optional to decrease the crust.
		ActivityType                  ActivityType
		TaskListName                  string
		Input                         []byte
		ScheduleToCloseTimeoutSeconds int32
		ScheduleToStartTimeoutSeconds int32
		StartToCloseTimeoutSeconds    int32
		HeartbeatTimeoutSeconds       int32
		WaitForCancellation           bool
	}

	// asyncActivityClient for requesting activity execution
	asyncActivityClient interface {
		// The ExecuteActivity schedules an activity with a callback handler.
		// If the activity failed to complete the callback error would indicate the failure
		// and it can be one of ActivityTaskFailedError, ActivityTaskTimeoutError, ActivityTaskCanceledError
		ExecuteActivity(parameters executeActivityParameters, callback resultHandler) *activityInfo

		// This only initiates cancel request for activity. if the activity is configured to not waitForCancellation then
		// it would invoke the callback handler immediately with error code ActivityTaskCanceledError.
		// If the activity is not running(either scheduled or started) then it is a no-operation.
		RequestCancelActivity(activityID string)
	}

	activityEnvironment struct {
		taskToken         []byte
		workflowExecution WorkflowExecution
		activityID        string
		activityType      ActivityType
		serviceInvoker    ServiceInvoker
		logger            *zap.Logger
	}

	// activityOptions stores all activity-specific parameters that will
	// be stored inside of a context.
	activityOptions struct {
		activityID                    *string
		taskListName                  *string
		scheduleToCloseTimeoutSeconds *int32
		scheduleToStartTimeoutSeconds *int32
		startToCloseTimeoutSeconds    *int32
		heartbeatTimeoutSeconds       *int32
		waitForCancellation           *bool
	}
)

// Assert that structs do indeed implement the interfaces
var _ ActivityOptions = (*activityOptions)(nil)

const activityEnvContextKey = "activityEnv"
const activityOptionsContextKey = "activityOptions"

func getActivityEnv(ctx context.Context) *activityEnvironment {
	env := ctx.Value(activityEnvContextKey)
	if env == nil {
		panic("getActivityEnv: Not an activity context")
	}
	return env.(*activityEnvironment)
}

func getActivityOptions(ctx Context) *executeActivityParameters {
	eap := ctx.Value(activityOptionsContextKey)
	if eap == nil {
		return nil
	}
	return eap.(*executeActivityParameters)
}

func getValidatedActivityOptions(ctx Context) (*executeActivityParameters, error) {
	p := getActivityOptions(ctx)
	if p == nil {
		// We need task list as a compulsory parameter. This can be removed after registration
		return nil, errActivityParamsBadRequest
	}
	if p.ScheduleToStartTimeoutSeconds <= 0 {
		return nil, errors.New("missing or negative ScheduleToStartTimeoutSeconds")
	}
	if p.ScheduleToCloseTimeoutSeconds <= 0 {
		return nil, errors.New("missing or negative ScheduleToCloseTimeoutSeconds")
	}
	if p.StartToCloseTimeoutSeconds <= 0 {
		return nil, errors.New("missing or negative StartToCloseTimeoutSeconds")
	}
	return p, nil
}

func validateFunctionArgs(f interface{}, args []interface{}, isWorkflow bool) error {
	fType := reflect.TypeOf(f)
	if fType.Kind() != reflect.Func {
		return fmt.Errorf("Provided type: %v is not a function type", f)
	}
	fnName := getFunctionName(f)

	fnArgIndex := 0
	// Skip Context function argument.
	if fType.NumIn() > 0 {
		if isWorkflow && isWorkflowContext(fType.In(0)) {
			fnArgIndex++
		}
		if !isWorkflow && isActivityContext(fType.In(0)) {
			fnArgIndex++
		}
	}

	// Validate provided args match with function order match.
	if fType.NumIn()-fnArgIndex != len(args) {
		return fmt.Errorf(
			"expected %d args for function: %v but found %v",
			fType.NumIn()-fnArgIndex, fnName, len(args))
	}

	for i := 0; fnArgIndex < fType.NumIn(); fnArgIndex, i = fnArgIndex+1, i+1 {
		fnArgType := fType.In(fnArgIndex)
		argType := reflect.TypeOf(args[i])
		if !argType.AssignableTo(fnArgType) {
			return fmt.Errorf(
				"cannot assign function argument: %d from type: %s to type: %s",
				fnArgIndex+1, argType, fnArgType,
			)
		}
	}

	return nil
}

func validateFunctionResults(f interface{}, result interface{}) ([]byte, error) {
	fType := reflect.TypeOf(f)
	switch fType.Kind() {
	case reflect.String:
		// With the name we can't validate. No operation.
	case reflect.Func:
		err := validateFnFormat(fType, false)
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf(
			"Invalid type 'f' parameter provided, it can be either activity function or name of the activity: %v", f)
	}

	if result == nil {
		return nil, nil
	}

	data, err := getHostEnvironment().encodeArg(result)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func getValidatedActivityFunction(f interface{}, args []interface{}) (*ActivityType, []byte, error) {
	fnName := ""
	fType := reflect.TypeOf(f)
	switch fType.Kind() {
	case reflect.String:
		fnName = reflect.ValueOf(f).String()

	case reflect.Func:
		if err := validateFunctionArgs(f, args, false); err != nil {
			return nil, nil, err
		}
		fnName = getFunctionName(f)

	default:
		return nil, nil, fmt.Errorf(
			"Invalid type 'f' parameter provided, it can be either activity function or name of the activity: %v", f)
	}

	input, err := getHostEnvironment().encodeArgs(args)
	if err != nil {
		return nil, nil, err
	}
	return &ActivityType{Name: fnName}, input, nil
}

func isActivityContext(inType reflect.Type) bool {
	contextElem := reflect.TypeOf((*context.Context)(nil)).Elem()
	return inType.Implements(contextElem)
}

func validateFunctionAndGetResults(f interface{}, values []reflect.Value) ([]byte, error) {
	fnName := getFunctionName(f)
	resultSize := len(values)

	if resultSize < 1 || resultSize > 2 {
		return nil, fmt.Errorf(
			"The function: %v signature returns %d results, it is expecting to return either error or (result, error)",
			fnName, resultSize)
	}

	var result []byte
	var err error

	// Parse result
	if resultSize > 1 {
		r := values[0].Interface()
		result, err = getHostEnvironment().encodeArg(r)
		if err != nil {
			return nil, err
		}
	}

	// Parse error.
	errValue := values[resultSize-1]
	if errValue.IsNil() {
		return result, nil
	}
	errInterface, ok := errValue.Interface().(error)
	if !ok {
		return nil, fmt.Errorf(
			"Failed to parse error result as it is not of error interface: %v",
			errValue)
	}
	return result, errInterface
}

func deSerializeFnResultFromFnType(fnType reflect.Type, result []byte, to interface{}) error {
	if fnType.Kind() != reflect.Func {
		return fmt.Errorf("expecting only function type but got type: %v", fnType)
	}

	// We already validated during registration that it either have (result, error) (or) just error.
	if fnType.NumOut() <= 1 {
		return nil
	} else if fnType.NumOut() == 2 {
		if result == nil {
			return nil
		}
		err := getHostEnvironment().decodeArg(result, to)
		if err != nil {
			return err
		}
	}
	return nil
}

func deSerializeFunctionResult(f interface{}, result []byte, to interface{}) error {
	fType := reflect.TypeOf(f)

	switch fType.Kind() {
	case reflect.Func:
		// We already validated that it either have (result, error) (or) just error.
		return deSerializeFnResultFromFnType(fType, result, to)

	case reflect.String:
		// If we know about this function through registration then we will try to return corresponding result type.
		fnName := reflect.ValueOf(f).String()
		if fnRegistered, ok := getHostEnvironment().getActivityFn(fnName); ok {
			return deSerializeFnResultFromFnType(reflect.TypeOf(fnRegistered), result, to)
		}
	}

	// For everything we return result.
	return getHostEnvironment().decodeArg(result, to)
}

func setActivityParametersIfNotExist(ctx Context) Context {
	if valCtx := getActivityOptions(ctx); valCtx == nil {
		return WithValue(ctx, activityOptionsContextKey, &executeActivityParameters{})
	}
	return ctx
}

// WithTaskList sets the task list name for this Context.
func (ab *activityOptions) WithTaskList(name string) ActivityOptions {
	ab.taskListName = common.StringPtr(name)
	return ab
}

// WithScheduleToCloseTimeout sets timeout for this Context.
func (ab *activityOptions) WithScheduleToCloseTimeout(d time.Duration) ActivityOptions {
	ab.scheduleToCloseTimeoutSeconds = common.Int32Ptr(int32(d.Seconds()))
	return ab
}

// WithScheduleToStartTimeout sets timeout for this Context.
func (ab *activityOptions) WithScheduleToStartTimeout(d time.Duration) ActivityOptions {
	ab.scheduleToStartTimeoutSeconds = common.Int32Ptr(int32(d.Seconds()))
	return ab
}

// WithStartToCloseTimeout sets timeout for this Context.
func (ab *activityOptions) WithStartToCloseTimeout(d time.Duration) ActivityOptions {
	ab.startToCloseTimeoutSeconds = common.Int32Ptr(int32(d.Seconds()))
	return ab
}

// WithHeartbeatTimeout sets timeout for this Context.
func (ab *activityOptions) WithHeartbeatTimeout(d time.Duration) ActivityOptions {
	ab.heartbeatTimeoutSeconds = common.Int32Ptr(int32(d.Seconds()))
	return ab
}

// WithWaitForCancellation sets timeout for this Context.
func (ab *activityOptions) WithWaitForCancellation(wait bool) ActivityOptions {
	ab.waitForCancellation = &wait
	return ab
}

// WithActivityID sets the activity task list ID for this Context.
// NOTE: We don't expose configuring activity ID to the user, This is something will be done in future
// so they have end to end scenario of how to use this ID to complete and fail an activity(business use case).
func (ab *activityOptions) WithActivityID(activityID string) ActivityOptions {
	ab.activityID = common.StringPtr(activityID)
	return ab
}