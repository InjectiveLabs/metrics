package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTiming(t *testing.T) {
	rec := record(t)
	fx := func(t *testing.T) (err error, str string, num int, flag bool) {
		defer func() {
			assert.Len(t, rec.calls, 1)
			assert.Equal(t, "Timing", rec.calls[0][0])
			assert.Equal(t, "metric_name", rec.calls[0][1])
			assert.GreaterOrEqual(t, rec.calls[0][2], 10*time.Millisecond)
			assert.Len(t, rec.calls[0][3], 9) // tags
			assert.ElementsMatch(t, []string{
				"error=true",
				"string_ref=something",
				"number_ref=42",
				"flag_ref=true",
				"string=nothing",
				"number=10",
				"flag=false",
				// From Tags
				"foo=bar",
				"baz=qux", // Overwritten by endTags
			}, rec.calls[0][3]) // tags
		}()

		// Setting initial values, they should be overwritten when the function returns
		err = nil
		str = "nothing"
		num = 10
		flag = false
		initialTags := Tags{"foo": "bar"}
		endTags := Tags{"baz": "qux"} // overwrite baz pair from tag

		defer TimingWithErr("metric_name", initialTags, "baz", "fox")(&err,
			"string", str, "number", num, "flag", endTags,
			flag, "string_ref", &str, "number_ref", &num, "flag_ref", &flag,
		)
		time.Sleep(time.Millisecond * 10)

		// these values should be sent to the metrics
		return errors.New("this error should be recorded"), "something", 42, true
	}

	err, str, num, flag := fx(t)
	require.Error(t, err)
	assert.Equal(t, "this error should be recorded", err.Error())
	assert.Equal(t, "something", str)
	assert.Equal(t, 42, num)
	assert.True(t, flag)
}

func TestToString(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected string
		ok       bool
	}{
		{"string", "test", "test", true},
		{"int", 42, "42", true},
		{"int8", int8(42), "42", true},
		{"int16", int16(42), "42", true},
		{"int32", int32(42), "42", true},
		{"int64", int64(42), "42", true},
		{"uint", uint(42), "42", true},
		{"uint8", uint8(42), "42", true},
		{"uint16", uint16(42), "42", true},
		{"uint32", uint32(42), "42", true},
		{"uint64", uint64(42), "42", true},
		{"float32", float32(42.42), "42.42", true},
		{"float64", float64(42.42), "42.42", true},
		{"bool", true, "true", true},
		{"*string", func() *string { s := "test"; return &s }(), "test", true},
		{"*int", func() *int { i := 42; return &i }(), "42", true},
		{"*int8", func() *int8 { i := int8(42); return &i }(), "42", true},
		{"*int16", func() *int16 { i := int16(42); return &i }(), "42", true},
		{"*int32", func() *int32 { i := int32(42); return &i }(), "42", true},
		{"*int64", func() *int64 { i := int64(42); return &i }(), "42", true},
		{"*uint", func() *uint { u := uint(42); return &u }(), "42", true},
		{"*uint8", func() *uint8 { u := uint8(42); return &u }(), "42", true},
		{"*uint16", func() *uint16 { u := uint16(42); return &u }(), "42", true},
		{"*uint32", func() *uint32 { u := uint32(42); return &u }(), "42", true},
		{"*uint64", func() *uint64 { u := uint64(42); return &u }(), "42", true},
		{"*float32", func() *float32 { f := float32(42.42); return &f }(), "42.42", true},
		{"*float64", func() *float64 { f := float64(42.42); return &f }(), "42.42", true},
		{"*bool", func() *bool { b := true; return &b }(), "true", true},
		{"nil", nil, "nil", true},
		{"unsupported type", struct{}{}, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := ToString(tt.input)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func record(t *testing.T) *statterRecorder {
	t.Helper()
	var rec statterRecorder
	_ = Init("", "", &StatterConfig{StuckFunctionTimeout: time.Minute, MockingEnabled: true})
	oldClient := client
	client = &rec
	t.Cleanup(func() {
		client = oldClient
	})
	return &rec

}

func TestCounter(t *testing.T) {
	expectedTags := []string{"foo=bar", "baz=qux"}
	assertSameResults := func(t *testing.T, rec *statterRecorder) {
		assert.Len(t, rec.calls, 1)
		assert.Equal(t, "Count", rec.calls[0][0])
		assert.Equal(t, "my-counter", rec.calls[0][1])
		assert.EqualValues(t, 5, rec.calls[0][2])
		assert.Len(t, rec.calls[0][3], 2)
		assert.ElementsMatch(t, expectedTags, rec.calls[0][3])
	}

	t.Run("using Tags", func(t *testing.T) {
		rec := record(t)
		Counter("my-counter", 5, Tags{"foo": "bar", "baz": "qux"})
		assertSameResults(t, rec)
	})

	t.Run("using multiple Tags", func(t *testing.T) {
		rec := record(t)
		Counter("my-counter", 5, Tags{"foo": "bar"}, Tags{"baz": "qux"})
		assertSameResults(t, rec)
	})

	t.Run("using pairs of key-value arguments", func(t *testing.T) {
		rec := record(t)
		Counter("my-counter", 5, "foo", "bar", "baz", "qux")
		assertSameResults(t, rec)
	})

	t.Run("using a combination of Tags and pairs of key-value arguments", func(t *testing.T) {
		rec := record(t)
		Counter("my-counter", 5, Tags{"foo": "bar"}, "baz", "qux")
		assertSameResults(t, rec)
	})
}

func TestIncr(t *testing.T) {
	expectedTags := []string{"foo=bar", "baz=qux"}
	assertSameResults := func(t *testing.T, rec *statterRecorder) {
		assert.Len(t, rec.calls, 1)
		assert.Equal(t, "Count", rec.calls[0][0])
		assert.Equal(t, "my-gauge", rec.calls[0][1])
		assert.EqualValues(t, 1, rec.calls[0][2])
		assert.Len(t, rec.calls[0][3], 2)
		assert.ElementsMatch(t, expectedTags, rec.calls[0][3])
	}

	t.Run("using Tags", func(t *testing.T) {
		rec := record(t)
		Incr("my-gauge", Tags{"foo": "bar", "baz": "qux"})
		assertSameResults(t, rec)
	})

	t.Run("using multiple Tags", func(t *testing.T) {
		rec := record(t)
		Incr("my-gauge", Tags{"foo": "bar"}, Tags{"baz": "qux"})
		assertSameResults(t, rec)
	})

	t.Run("using pairs of key-value arguments", func(t *testing.T) {
		rec := record(t)
		Incr("my-gauge", "foo", "bar", "baz", "qux")
		assertSameResults(t, rec)
	})

	t.Run("using a combination of Tags and pairs of key-value arguments", func(t *testing.T) {
		rec := record(t)
		Incr("my-gauge", Tags{"foo": "bar"}, "baz", "qux")
		assertSameResults(t, rec)
	})
}

func TestGauge(t *testing.T) {
	expectedTags := []string{"foo=bar", "baz=qux"}
	assertSameResults := func(t *testing.T, rec *statterRecorder) {
		assert.Len(t, rec.calls, 1)
		assert.Equal(t, "Gauge", rec.calls[0][0])
		assert.Equal(t, "my-gauge", rec.calls[0][1])
		assert.EqualValues(t, 5, rec.calls[0][2])
		assert.Len(t, rec.calls[0][3], 2)
		assert.ElementsMatch(t, expectedTags, rec.calls[0][3])
	}

	t.Run("using Tags", func(t *testing.T) {
		rec := record(t)
		Gauge("my-gauge", 5, Tags{"foo": "bar", "baz": "qux"})
		assertSameResults(t, rec)
	})

	t.Run("using multiple Tags", func(t *testing.T) {
		rec := record(t)
		Gauge("my-gauge", 5, Tags{"foo": "bar"}, Tags{"baz": "qux"})
		assertSameResults(t, rec)
	})

	t.Run("using pairs of key-value arguments", func(t *testing.T) {
		rec := record(t)
		Gauge("my-gauge", 5, "foo", "bar", "baz", "qux")
		assertSameResults(t, rec)
	})

	t.Run("using a combination of Tags and pairs of key-value arguments", func(t *testing.T) {
		rec := record(t)
		Gauge("my-gauge", 5, Tags{"foo": "bar"}, "baz", "qux")
		assertSameResults(t, rec)
	})
}

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
		assert.Equal(t, "Timing", rec.calls[1][0])
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
		assert.Equal(t, "Timing", rec.calls[1][0])
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
	r.calls = append(r.calls, []interface{}{"Decr", name, tags, rate})
	return nil
}

func (r *statterRecorder) Gauge(name string, value float64, tags []string, rate float64) error {
	r.calls = append(r.calls, []interface{}{"Gauge", name, value, tags, rate})
	return nil
}

func (r *statterRecorder) Timing(name string, value time.Duration, tags []string, rate float64) error {
	r.calls = append(r.calls, []interface{}{"Timing", name, value, tags, rate})
	return nil
}

func (r *statterRecorder) Histogram(name string, value float64, tags []string, rate float64) error {
	r.calls = append(r.calls, []interface{}{"Histogram", name, value, tags, rate})
	return nil
}

func (r *statterRecorder) Close() error {
	r.calls = append(r.calls, []interface{}{"Close"})
	return nil
}
