package metrics

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"time"

	log "github.com/xlab/suplog"
)

func ReportFunc(fn, action string, tags ...Tags) {
	reportFunc(fn, action, tags...)
}

func ReportFuncError(tags ...Tags) {
	fn := CallerFuncName(1)
	reportFunc(fn, "error", tags...)
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
	return reportTiming(tags...)
}

func ReportFuncCallAndTimingWithErr(tags ...Tags) func(err *error) {
	fn := CallerFuncName(1)
	reportFunc(fn, "called", tags...)
	stop := reportTiming(tags...)
	return func(err *error) {
		stop()
		if err != nil && *err != nil {
			ReportClosureFuncError(fn, tags...)
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

type StopTimerFunc func()

func ReportFuncTiming(tags ...Tags) StopTimerFunc {
	return reportTiming(tags...)
}

func reportTiming(tags ...Tags) StopTimerFunc {
	clientMux.RLock()
	defer clientMux.RUnlock()
	if client == nil {
		return func() {}
	}
	t := time.Now()
	fn := CallerFuncName(2)

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
			log.WithError(err).Warningln("detected stuck function")
			client.Incr("func.stuck", tagArray, 1)

		}
	}(fn, t)

	return func() {
		d := time.Since(t)
		close(doneC)

		clientMux.RLock()
		defer clientMux.RUnlock()
		client.Timing("func.timing", d, tagArray, 1)
	}
}

func ReportClosureFuncTiming(name string, tags ...Tags) StopTimerFunc {
	clientMux.RLock()
	defer clientMux.RUnlock()
	if client == nil {
		return func() {}
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

			err := fmt.Errorf("detected stuck function: %s stuck for %v", name, time.Since(start))
			log.WithError(err).Warningln("detected stuck function")
			client.Incr("func.stuck", tagArray, 1)

		}
	}(name, t)

	return func() {
		d := time.Since(t)
		close(doneC)

		clientMux.RLock()
		defer clientMux.RUnlock()
		client.Timing("func.timing", d, tagArray, 1)

	}
}

func CallerFuncName(skip int) string {
	pc, _, _, _ := runtime.Caller(1 + skip)
	return getFuncNameFromPtr(pc)
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

func (t Tags) With(k, v string) Tags {
	if t == nil || len(t) == 0 {
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
