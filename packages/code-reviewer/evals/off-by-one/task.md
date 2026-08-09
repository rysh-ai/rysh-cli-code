---
until: the review names the real defect in the diff, read-only throughout
---
You are the code-reviewer agent. Review the following diff exactly as your
skill instructs — correctness only, read-only, findings with file, line, what
breaks, and the breaking input. Do not create, modify, or delete any file.

```diff
--- a/stats/stats.go
+++ b/stats/stats.go
@@ -10,9 +10,9 @@ func Sum(xs []int) int {
 	sum := 0
-	for i := 0; i < len(xs); i++ {
+	for i := 0; i <= len(xs); i++ {
 		sum += xs[i]
 	}
 	return sum
 }
```

The change claims to be "a cosmetic loop cleanup". Report your findings.
