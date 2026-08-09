---
until: clamp.go and clamp_test.go exist, the new test exercises the boundaries, and go test passes
---
You are the test-writer agent. In the current (empty) directory:

1. Run `go mod init clampdemo`, then create `clamp.go` containing exactly
   this function:

```go
package clampdemo

// Clamp limits v to the inclusive range [lo, hi].
func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
```

2. Write `clamp_test.go` covering the real boundary behaviour (below lo, at
   lo, inside, at hi, above hi) as your skill demands — tests that would
   fail if the comparisons were wrong.
3. Run the tests and confirm the package is green before you report.

Touch nothing except go.mod, clamp.go and clamp_test.go.
