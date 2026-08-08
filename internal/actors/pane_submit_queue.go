package actors

import "sync"

// ptySubmitQueue serializes the PTY writes made by submitToPTY, in the order
// the pane accepted the commands.
//
// It exists because a submission to an interactive pane is not a single write:
// the command text goes out first and the Enter follows a beat later, so the
// TUI's paste detection cannot swallow it (see submitToPTY). Without a queue,
// two commands arriving back-to-back would interleave as text1, text2, CR, CR —
// the second prompt appended to the first, then submitted twice.
//
// Work runs on a goroutine, so the actor never blocks on the inter-write pause;
// enqueue starts one on demand and it exits as soon as the queue drains, so an
// idle pane holds no goroutine. Queued work must therefore not touch actor
// state: submitToPTY captures the PTY handle and the interactive decision on
// the mailbox goroutine, and reports failures back through NATS.
type ptySubmitQueue struct {
	mu      sync.Mutex
	pending []func()
	running bool
}

// enqueue appends fn and makes sure a drain goroutine is running. It returns
// immediately; fn runs after every previously enqueued function has finished.
func (q *ptySubmitQueue) enqueue(fn func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, fn)
	if !q.running {
		q.running = true
		go q.drain()
	}
}

// drain runs queued work until the queue is empty, then clears the running flag
// under the lock so a concurrent enqueue either sees running (and lets this
// goroutine pick its item up) or starts a fresh one — never both, never neither.
func (q *ptySubmitQueue) drain() {
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		fn := q.pending[0]
		q.pending = q.pending[1:]
		q.mu.Unlock()

		fn()
	}
}
