package metrics

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	log "github.com/InjectiveLabs/suplog"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/mixpanel/mixpanel-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func ReportFunc(fn, action string, tags ...Tags) {
	reportFunc(fn, action, tags...)
}

func ReportFuncError(tags ...Tags) {
	fn := CallerFuncName(1)
	reportFunc(fn, "error", tags...)
}

func ReportFuncDeferredError(tags ...Tags) func(err *error, tags ...Tags) {
	fn := CallerFuncName(1)
	return func(err *error, stopTags ...Tags) {
		if err != nil && *err != nil {
			ReportClosureFuncError(fn, MergeTags(MergeTags(nil, tags...), stopTags...))
		}
	}
}

func ReportClosureFuncError(name string, tags ...Tags) {
	reportFunc(name, "error", tags...)
}

func ReportFuncStatus(tags ...Tags) {
	fn := CallerFuncName(1)
	reportFunc(fn, "status", tags...)
}

func ReportClosureFuncStatus(name string, tags ...Tags) {
	reportFunc(name, "status", tags...)
}

func ReportFuncCall(tags ...Tags) {
	fn := CallerFuncName(1)
	reportFunc(fn, "called", tags...)
}

func ReportFuncCallAndTiming(tags ...Tags) StopTimerFunc {
	fn := CallerFuncName(1)
	reportFunc(fn, "called", tags...)
	_, stopFn := reportTiming(context.Background(), fn, tags...)
	return stopFn
}

func ReportFuncCallAndTimingCtx(ctx context.Context, tags ...Tags) (context.Context, StopTimerFunc) {
	fn := CallerFuncName(1)
	reportFunc(fn, "called", tags...)
	return reportTiming(ctx, fn, tags...)
}

func ReportFuncCallAndTimingSdkCtx(sdkCtx sdk.Context, tags ...Tags) (sdk.Context, StopTimerFunc) {
	fn := CallerFuncName(1)
	reportFunc(fn, "called", tags...)
	spanCtx, doneFn := reportTiming(sdkCtx.Context(), fn, tags...)
	return sdkCtx.WithContext(spanCtx), doneFn
}

func ReportFuncCallAndTimingCtxWithErr(ctx context.Context, tags ...Tags) func(err *error, stopTags ...Tags) {
	return ReportNamedFuncCallAndTimingCtxWithErr(ctx, CallerFuncName(1), tags...)
}

func ReportNamedFuncCallAndTimingCtxWithErr(ctx context.Context, fn string, tags ...Tags) func(err *error, stopTags ...Tags) {
	reportFunc(fn, "called", tags...)
	_, stop := reportTiming(ctx, fn, tags...)
	return func(err *error, stopTags ...Tags) {
		finalTags := MergeTags(MergeTags(nil, tags...), stopTags...)
		stop(finalTags)
		if err != nil && *err != nil {
			ReportClosureFuncError(fn, finalTags)
		}
	}
}

func ReportFuncCallAndTimingWithErr(tags ...Tags) func(err *error, tags ...Tags) {
	fn := CallerFuncName(1)
	return ReportNamedFuncCallAndTimingWithErr(fn, tags...)
}

func ReportNamedFuncCallAndTimingWithErr(fn string, tags ...Tags) func(err *error, tags ...Tags) {
	reportFunc(fn, "called", tags...)
	_, stop := reportTiming(context.Background(), fn, tags...)
	return func(err *error, stopTags ...Tags) {
		stop(stopTags...)
		if err != nil && *err != nil {
			finalTags := MergeTags(MergeTags(nil, tags...), stopTags...)
			ReportClosureFuncError(fn, finalTags)
		}
	}
}

func ReportClosureFuncCall(name string, tags ...Tags) {
	reportFunc(name, "called", tags...)
}

func reportFunc(fn, action string, tags ...Tags) {
	clientMux.RLock()
	defer clientMux.RUnlock()
	if client == nil {
		return
	}

	tagArray := JoinTags(tags...)
	tagArray = append(tagArray, getSingleTag("func_name", fn))
	client.Incr(fmt.Sprintf("func.%v", action), tagArray, 0.77)
}

type StopTimerFunc func(tags ...Tags)

func ReportFuncTiming(tags ...Tags) StopTimerFunc {
	_, stopFn := reportTiming(context.Background(), CallerFuncName(1), tags...)
	return stopFn
}

func ReportFuncTimingCtx(ctx context.Context, tags ...Tags) (context.Context, StopTimerFunc) {
	return reportTiming(ctx, CallerFuncName(1), tags...)
}

func reportTiming(ctx context.Context, fn string, tags ...Tags) (context.Context, StopTimerFunc) {
	clientMux.RLock()
	defer clientMux.RUnlock()

	if client == nil {
		return ctx, func(...Tags) {}
	}
	t := time.Now()

	if fn == "" {
		fn = CallerFuncName(2)
	}

	var (
		span    trace.Span
		spanCtx = ctx
	)
	if tracer != nil {
		spanCtx, span = tracer.Start(ctx, fn)
		for _, tags := range tags {
			for k, v := range tags {
				span.SetAttributes(attribute.String(k, v))
			}
		}
	}

	tagArray := JoinTags(tags...)
	tagArray = append(tagArray, getSingleTag("func_name", fn))

	doneC := make(chan struct{})
	go func(name string, start time.Time) {
		timeout := time.NewTimer(config.StuckFunctionTimeout)
		defer timeout.Stop()

		select {
		case <-doneC:
			return
		case <-timeout.C:
			clientMux.RLock()
			defer clientMux.RUnlock()

			err := fmt.Errorf("detected stuck function: %s stuck for %v", name, time.Since(start))
			fmt.Println(err)
			client.Incr("func.stuck", tagArray, 1)
			if span != nil {
				span.SetStatus(codes.Error, "stuck")
				span.End()
			}
		}
	}(fn, t)

	return spanCtx, func(stopTags ...Tags) {
		d := time.Since(t)
		close(doneC)

		stopTagArray := append(tagArray, JoinTags(stopTags...)...)

		clientMux.RLock()
		defer clientMux.RUnlock()
		client.Timing("func.timing", d, stopTagArray, 1)
		if span != nil {
			span.End()
		}
	}
}

func ReportClosureFuncTiming(name string, tags ...Tags) StopTimerFunc {
	clientMux.RLock()
	defer clientMux.RUnlock()
	if client == nil {
		return func(...Tags) {}
	}
	t := time.Now()
	tagArray := JoinTags(tags...)
	tagArray = append(tagArray, getSingleTag("func_name", name))

	doneC := make(chan struct{})
	go func(name string, start time.Time) {
		timeout := time.NewTimer(config.StuckFunctionTimeout)
		defer timeout.Stop()

		select {
		case <-doneC:
			return
		case <-timeout.C:
			clientMux.RLock()
			defer clientMux.RUnlock()

			log.Warningf("detected stuck function: %s stuck for %v", name, time.Since(start))
			client.Incr("func.stuck", tagArray, 1)
		}
	}(name, t)

	return func(stopTags ...Tags) {
		d := time.Since(t)
		close(doneC)
		stopTagArray := append(tagArray, JoinTags(stopTags...)...)

		clientMux.RLock()
		defer clientMux.RUnlock()
		client.Timing("func.timing", d, stopTagArray, 1)
	}
}

func CallerFuncName(skip int) string {
	pc, _, _, _ := runtime.Caller(1 + skip)
	return getFuncNameFromPtr(pc)
}

func Track(ctx context.Context, events []*mixpanel.Event) error {
	if mixPanelClient != nil {
		err := mixPanelClient.Track(ctx, events)
		if err != nil {
			return err
		}
	}

	return nil
}

func NewEvent(name string, distinctID string, properties map[string]any) *mixpanel.Event {
	if mixPanelClient != nil {
		return mixPanelClient.NewEvent(name, distinctID, properties)
	}

	return nil
}

func GetFuncName(i interface{}) string {
	return getFuncNameFromPtr(reflect.ValueOf(i).Pointer())
}

func getFuncNameFromPtr(ptr uintptr) string {
	fullName := runtime.FuncForPC(ptr).Name()
	parts := strings.Split(fullName, "/")
	if len(parts) == 0 {
		return ""
	}
	nameParts := strings.Split(parts[len(parts)-1], ".")
	if len(nameParts) == 0 {
		return ""
	}
	return strings.TrimSuffix(nameParts[len(nameParts)-1], "-fm")
}

type Tags map[string]string

func MergeTags(original Tags, src ...Tags) Tags {
	dst := make(Tags)
	for k, v := range original {
		dst[k] = v
	}
	for _, tags := range src {
		for k, v := range tags {
			dst[k] = v
		}
	}
	return dst
}

func AddPairs(t Tags, tags ...interface{}) Tags {
	if t == nil {
		t = make(Tags)
	}
	for i := 0; i < len(tags); i += 2 {
		if i+1 >= len(tags) {
			break
		}
		k, ok := tags[i].(string)
		if !ok {
			continue
		}

		// special case for nil scalars
		if tags[i+1] == nil {
			t[k] = "nil"
			continue
		}

		v, ok := ToString(tags[i+1])
		if !ok {
			continue
		}
		t[k] = v
	}
	return t
}

func Combine(tags ...interface{}) Tags {
	tt := make(Tags)
	var pairs []interface{}
	for _, x := range tags {
		switch v := x.(type) {
		case Tags:
			for k, val := range v {
				tt[k] = val
			}
		case []interface{}:
			AddPairs(tt, v...)
		default:
			pairs = append(pairs, v)
		}
	}
	return AddPairs(tt, pairs...)
}

func (t Tags) AddPairs(tags ...interface{}) Tags {
	return AddPairs(t, tags...)
}

func (t Tags) With(k, v string) Tags {
	if len(t) == 0 {
		return map[string]string{
			k: v,
		}
	}
	t[k] = v
	return t
}

func joinTelegrafTags(tags ...Tags) []string {
	if len(tags) == 0 {
		return []string{}
	}

	tagArray := make([]string, len(tags[0]))
	i := 0
	for k, v := range tags[0] {
		tag := fmt.Sprintf("%s=%s", k, v)
		tagArray[i] = tag
		i += 1
	}
	return tagArray
}

func joinDDTags(tags ...Tags) []string {
	if len(tags) == 0 {
		return []string{}
	}
	tagArray := make([]string, len(tags[0]))
	i := 0
	for k, v := range tags[0] {
		tag := fmt.Sprintf("%s:%s", k, v)
		tagArray[i] = tag
		i += 1
	}
	return tagArray
}

// JoinTags decides how to join tags base on agent
func JoinTags(tags ...Tags) []string {
	if config.Agent == DatadogAgent {
		return joinDDTags(tags...)
	}

	return joinTelegrafTags(tags...)
}

func getSingleTag(key, value string) string {
	if config.Agent == DatadogAgent {
		return fmt.Sprintf("%s:%s", key, value)
	}

	return fmt.Sprintf("%s=%s", key, value)
}

// ToString converts various types to string in the most efficient (and verbose) way possible.
func ToString(i interface{}) (v string, ok bool) {
	ok = true
	switch x := i.(type) {
	case string:
		v = x
	case int:
		v = strconv.Itoa(x)
	case int8:
		v = strconv.FormatInt(int64(x), 10)
	case int16:
		v = strconv.FormatInt(int64(x), 10)
	case int32:
		v = strconv.FormatInt(int64(x), 10)
	case int64:
		v = strconv.FormatInt(x, 10)
	case uint:
		v = strconv.FormatUint(uint64(x), 10)
	case uint8:
		v = strconv.FormatUint(uint64(x), 10)
	case uint16:
		v = strconv.FormatUint(uint64(x), 10)
	case uint32:
		v = strconv.FormatUint(uint64(x), 10)
	case uint64:
		v = strconv.FormatUint(x, 10)
	case float32:
		v = strconv.FormatFloat(float64(x), 'f', -1, 32)
	case float64:
		v = strconv.FormatFloat(x, 'f', -1, 64)
	case *string:
		if x != nil {
			v = *x
		}
	case *int:
		if x != nil {
			v = strconv.FormatInt(int64(*x), 10)
		}
	case *int8:
		if x != nil {
			v = strconv.FormatInt(int64(*x), 10)
		}
	case *int16:
		if x != nil {
			v = strconv.FormatInt(int64(*x), 10)
		}
	case *int32:
		if x != nil {
			v = strconv.FormatInt(int64(*x), 10)
		}
	case *int64:
		if x != nil {
			v = strconv.FormatInt(*x, 10)
		}
	case *uint:
		if x != nil {
			v = strconv.FormatUint(uint64(*x), 10)
		}
	case *uint8:
		if x != nil {
			v = strconv.FormatUint(uint64(*x), 10)
		}
	case *uint16:
		if x != nil {
			v = strconv.FormatUint(uint64(*x), 10)
		}
	case *uint32:
		if x != nil {
			v = strconv.FormatUint(uint64(*x), 10)
		}
	case *uint64:
		if x != nil {
			v = strconv.FormatUint(*x, 10)
		}
	case *float32:
		if x != nil {
			v = strconv.FormatFloat(float64(*x), 'f', -1, 32)
		}
	case *float64:
		if x != nil {
			v = strconv.FormatFloat(*x, 'f', -1, 64)
		}
	case bool:
		v = strconv.FormatBool(x)
	case *bool:
		v = strconv.FormatBool(*x)
	case nil:
		v = "nil"
	default:
		ok = false
	}
	return v, ok
}
