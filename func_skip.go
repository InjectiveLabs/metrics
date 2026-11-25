package metrics

import "strings"

// funcSkipSet returns the function names a meter with the given scope name
// must not time, taken from Config.FuncTimingSkip. An entry is either a bare
// function name, which applies to every scope, or "scope.Function", which
// applies to that scope only. It returns nil when nothing applies.
func funcSkipSet(scope string, entries []string) map[string]struct{} {
	var set map[string]struct{}

	add := func(fn string) {
		if fn == "" {
			return
		}
		if set == nil {
			set = make(map[string]struct{})
		}
		set[fn] = struct{}{}
	}

	prefix := scope + "."
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		// A function name may itself contain a dot ("Engine.Evict"), so an
		// entry counts both as a bare name and, when it starts with this
		// scope, as a scoped one.
		add(entry)
		if scope != "" && strings.HasPrefix(entry, prefix) {
			add(strings.TrimPrefix(entry, prefix))
		}
	}

	return set
}

// skipsFuncTiming reports whether FuncTiming and FuncTimingCtx do nothing for
// the function: no call counter, no span, no timing histogram, no error
// report from the stop function.
func (m *meter) skipsFuncTiming(fn string) bool {
	_, skip := m.skipFuncs[fn]
	return skip
}
