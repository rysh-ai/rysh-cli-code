---
until: the failing test passes because the implementation defect was fixed, suite green
---
You are the test-fixer agent. In the current (empty) directory, set up this
tiny module and fix it:

1. Run `go mod init fixdemo`, then create these two files exactly as given.

`round.go`:

```go
package fixdemo

// RoundUp returns n rounded up to the next multiple of step.
// RoundUp(10, 5) == 10, RoundUp(11, 5) == 15.
func RoundUp(n, step int) int {
	return (n / step) * step
}
```

`round_test.go`:

```go
package fixdemo

import "testing"

func TestRoundUp(t *testing.T) {
	cases := []struct{ n, step, want int }{
		{10, 5, 10},
		{11, 5, 15},
		{1, 4, 4},
	}
	for _, c := range cases {
		if got := RoundUp(c.n, c.step); got != c.want {
			t.Errorf("RoundUp(%d,%d) = %d, want %d", c.n, c.step, got, c.want)
		}
	}
}
```

2. Run the tests, read the failure, and fix it following your skill: the
   test is the specification — the defect is in the implementation. Do not
   change round_test.go.
3. Re-run until the package is green, and report before/after.
