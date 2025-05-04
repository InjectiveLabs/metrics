package metrics

import (
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func func1() string {
	return func2()
}

func func2() string {
	return func3()
}

func func3() string {
	return CallerFuncName(2)
}

func TestCallerFuncName(t *testing.T) {
	func1Name := func1()
	assert.Equal(t, func1Name, "func1")
}

func TestGetFuncName(t *testing.T) {
	func1Name := GetFuncName(func1)
	assert.Equal(t, func1Name, "func1")
}

func Test_ReportTimedFuncWithError(t *testing.T) {
	var rec statterRecorder

	_ = Init("", "", &StatterConfig{StuckFunctionTimeout: time.Minute, MockingEnabled: true})
	oldClient := client
	client = &rec
	defer func() {
		client = oldClient
	}()

	t.Run("can be deferred and report the error", func(t *testing.T) {
		rec.reset()
		exec := func() (err error) {
			defer ReportFuncCallAndTimingWithErr(Tags{"foo": "bar"})(&err, Tags{"stop": "error"})

			time.Sleep(5 * time.Millisecond)
			return errors.New("this error should be recorded")
		}

		require.Error(t, exec())
		require.Len(t, rec.calls, 3)

		expectedTags := []string{"foo=bar", "func_name=1"}
		assert.Equal(t, "Incr", rec.calls[0][0])
		assert.Equal(t, "func.called", rec.calls[0][1])
		assert.ElementsMatch(t, expectedTags, rec.calls[0][2])

		expectedTags = []string{"foo=bar", "stop=error", "func_name=1"}
		assert.Equal(t, "Count", rec.calls[1][0])
		assert.Equal(t, "func.timing", rec.calls[1][1])
		assert.ElementsMatch(t, expectedTags, rec.calls[1][3])

		assert.Equal(t, "Incr", rec.calls[2][0])
		assert.Equal(t, "func.error", rec.calls[2][1])
		assert.ElementsMatch(t, expectedTags, rec.calls[2][2])
	})

	t.Run("can be deferred and skip error reporting if nil", func(t *testing.T) {
		rec.reset()
		exec := func() {
			var err error
			defer ReportFuncCallAndTimingWithErr(Tags{"foo": "bar"})(&err)
		}

		exec()
		require.Len(t, rec.calls, 2)

		assert.Equal(t, "func.called", rec.calls[0][1])
		assert.Equal(t, "func.timing", rec.calls[1][1])
	})

	t.Run("can use a specific name", func(t *testing.T) {
		rec.reset()
		exec := func() (err error) {
			defer ReportNamedFuncCallAndTimingWithErr("customFunc", Tags{"foo": "bar"})(&err, Tags{"stop": "error"})

			time.Sleep(5 * time.Millisecond)
			return errors.New("this error should be recorded")
		}

		require.Error(t, exec())
		require.Len(t, rec.calls, 3)

		expectedTags := []string{"foo=bar", "func_name=customFunc"}
		assert.Equal(t, "Incr", rec.calls[0][0])
		assert.Equal(t, "func.called", rec.calls[0][1])
		assert.ElementsMatch(t, expectedTags, rec.calls[0][2])

		expectedTags = []string{"foo=bar", "stop=error", "func_name=customFunc"}
		assert.Equal(t, "Count", rec.calls[1][0])
		assert.Equal(t, "func.timing", rec.calls[1][1])
		assert.ElementsMatch(t, expectedTags, rec.calls[1][3])

		assert.Equal(t, "Incr", rec.calls[2][0])
		assert.Equal(t, "func.error", rec.calls[2][1])
		assert.ElementsMatch(t, expectedTags, rec.calls[2][2])
	})

	t.Run("can be deferred with new tags and skip error reporting", func(t *testing.T) {
		rec.reset()
		exec := func() {
			var err error
			defer ReportFuncCallAndTimingWithErr(Tags{"foo": "bar"})(&err, Tags{"something": "new"})
		}

		exec()
		require.Len(t, rec.calls, 2)

		expectedTags := []string{"foo=bar", "something=new", "func_name=1"}
		assert.Equal(t, "func.called", rec.calls[0][1])
		assert.Equal(t, "func.timing", rec.calls[1][1])
		assert.ElementsMatch(t, expectedTags, rec.calls[1][3])
	})
}

type statterRecorder struct {
	calls [][]interface{}
}

func (r *statterRecorder) reset() {
	r.calls = make([][]interface{}, 0)
}

func (r *statterRecorder) Count(name string, value int64, tags []string, rate float64) error {
	r.calls = append(r.calls, []interface{}{"Count", name, value, tags, rate})
	return nil
}

func (r *statterRecorder) Incr(name string, tags []string, rate float64) error {
	r.calls = append(r.calls, []interface{}{"Incr", name, tags, rate})
	return nil
}

func (r *statterRecorder) Decr(name string, tags []string, rate float64) error {
	r.calls = append(r.calls, []interface{}{"Count", name, tags, rate})
	return nil
}

func (r *statterRecorder) Gauge(name string, value float64, tags []string, rate float64) error {
	r.calls = append(r.calls, []interface{}{"Count", name, value, tags, rate})
	return nil
}

func (r *statterRecorder) Timing(name string, value time.Duration, tags []string, rate float64) error {
	r.calls = append(r.calls, []interface{}{"Count", name, value, tags, rate})
	return nil
}

func (r *statterRecorder) Histogram(name string, value float64, tags []string, rate float64) error {
	r.calls = append(r.calls, []interface{}{"Count", name, value, tags, rate})
	return nil
}

func (r *statterRecorder) Close() error {
	r.calls = append(r.calls, []interface{}{"Close"})
	return nil
}
