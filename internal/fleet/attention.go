// SPDX-License-Identifier: Apache-2.0

package fleet

// ObserveAttention records whether a member's pane needs a human, and why
// (root-order-044 #3; "" when it does not). It is observed like Activity and
// never touches declared work: design 054's two axes stay separate.
func (r *Registry) ObserveAttention(paneID, attention string, now int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.fleets {
		for i := range f.Members {
			m := &f.Members[i]
			if m.PaneID != paneID {
				continue
			}
			if m.Status == nil {
				m.Status = &MemberStatus{JoinedAt: now}
			}
			m.Status.Attention = attention
		}
	}
}
