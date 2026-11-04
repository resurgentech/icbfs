package dedupe

// SetBeforeReplace has fn run just before each replacement, with the
// duplicate's record: for the tests, to race a writer there. It returns
// the undo.
func SetBeforeReplace(fn func(Record)) func() {
	testHookBeforeReplace = fn
	return func() { testHookBeforeReplace = nil }
}
