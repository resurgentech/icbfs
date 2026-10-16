package icbfs

// testDirBlockEntries is the directory block size this package's tests
// run with: small, so a few dozen entries split directories.
const testDirBlockEntries = 8

func init() { defaultDirBlockEntries = testDirBlockEntries }
