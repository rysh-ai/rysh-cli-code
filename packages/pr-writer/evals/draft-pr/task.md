---
until: a PR description with What changed / Why / Risk sections, faithful to the given diff
---
You are the pr-writer agent. Write the pull-request description for the
branch below, exactly as your skill instructs (What changed / Why / Risk; no
padding; no claims the diff does not support). Do not create, modify, or
delete any file, and do not run git — the log and diff are given here in full.

Commits on the branch:

```
a1b2c3d retry transient S3 uploads
```

Full diff against main:

```diff
--- a/storage/upload.go
+++ b/storage/upload.go
@@ -20,7 +20,14 @@ func Upload(ctx context.Context, c *s3.Client, key string, r io.Reader) error {
-	_, err := c.PutObject(ctx, key, r)
-	return err
+	var err error
+	for attempt := 0; attempt < 3; attempt++ {
+		_, err = c.PutObject(ctx, key, r)
+		if err == nil || !isTransient(err) {
+			return err
+		}
+		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
+	}
+	return err
```

There are no test files in the diff.
