---
until: one loop pass delivers the feature with a test that covers it, go test and go vet green
---
You are running one do-pass of the softdev loop in the current (empty)
directory. The goal for this pass:

Create a Go module `slugdemo` with a function
`Slugify(s string) string` that lowercases s, replaces every run of
non-alphanumeric characters with a single "-", and trims leading/trailing
"-" (so `Slugify("Hello,  World!")` == "hello-world").

Follow the loop's pass discipline:

1. Implement the smallest honest slice (`go mod init slugdemo`, `slug.go`).
2. Add a test that fails without the behaviour (`slug_test.go`) — cover the
   run-collapsing and the trimming, not just the happy path.
3. Run `go test ./...` and `go vet ./...`; fix what they report.
4. Report: what changed, what is verified green, what remains.

Do not commit or push; touch only go.mod, slug.go and slug_test.go.
