// SPDX-License-Identifier: Apache-2.0

package facetime

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type resultMsg struct {
	reply      Reply
	err        error
	generation int
	operation  string
}
type picker struct {
	client     Client
	ctx        context.Context
	contacts   []Contact
	selected   *Contact
	cursor     int
	offset     int
	next       *int
	query      string
	searching  bool
	busy       bool
	generation int
	status     string
}

func NewPicker(ctx context.Context, c Client) tea.Model {
	return picker{client: c, ctx: ctx, busy: true}
}
func (p picker) Init() tea.Cmd {
	return p.request(Request{Version: Version, Operation: "contacts", Limit: 50})
}
func (p picker) request(r Request) tea.Cmd {
	return func() tea.Msg {
		reply, err := p.client.Do(p.ctx, r)
		return resultMsg{reply, err, p.generation, r.Operation}
	}
}
func (p picker) load() (tea.Model, tea.Cmd) {
	p.busy = true
	p.contacts = nil
	p.selected = nil
	p.cursor = 0
	p.status = ""
	p.generation++
	return p, p.request(Request{Version: Version, Operation: "contacts", Search: p.query, Offset: p.offset, Limit: 50})
}
func (p picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case resultMsg:
		if m.generation != p.generation {
			return p, nil
		}
		p.busy = false
		if m.err != nil {
			p.contacts = nil
			p.selected = nil
			p.status = m.err.Error()
			return p, nil
		}
		if m.operation == "call" {
			if m.reply.Opened {
				p.status = "Opened FaceTime audio. Use the Apple call window for controls."
			} else {
				p.status = "Call canceled."
			}
			return p, nil
		}
		p.contacts = m.reply.Contacts
		p.next = m.reply.NextOffset
		p.cursor = 0
	case tea.KeyMsg:
		key := m.String()
		if key == "ctrl+c" {
			return p, tea.Quit
		}
		if key == "esc" {
			if p.searching {
				p.searching = false
				return p, nil
			}
			if p.selected != nil && !p.busy {
				p.selected = nil
				p.cursor = 0
				p.status = ""
				return p, nil
			}
			return p, tea.Quit
		}
		if p.busy {
			return p, nil
		}
		if p.searching {
			switch key {
			case "enter":
				p.searching = false
				p.offset = 0
				return p.load()
			case "backspace":
				rs := []rune(p.query)
				if len(rs) > 0 {
					p.query = string(rs[:len(rs)-1])
				}
			default:
				if m.Type == tea.KeyRunes && len(p.query)+len(string(m.Runes)) <= 256 {
					p.query += Display(string(m.Runes))
				}
			}
			return p, nil
		}
		size := len(p.contacts)
		if p.selected != nil {
			size = len(p.selected.Addresses)
		}
		switch key {
		case "/":
			p.searching = true
		case "r":
			p.offset = 0
			return p.load()
		case "n":
			if p.selected == nil && p.next != nil {
				p.offset = *p.next
				return p.load()
			}
		case "p":
			if p.selected == nil && p.offset > 0 {
				p.offset -= 50
				if p.offset < 0 {
					p.offset = 0
				}
				return p.load()
			}
		case "up", "k":
			if p.cursor > 0 {
				p.cursor--
			}
		case "down", "j":
			if p.cursor+1 < size {
				p.cursor++
			}
		case "enter":
			if size == 0 {
				return p, nil
			}
			if p.selected == nil {
				c := p.contacts[p.cursor]
				p.selected = &c
				p.cursor = 0
				p.status = ""
				return p, nil
			}
			a := p.selected.Addresses[p.cursor]
			p.busy = true
			p.status = "Confirm the recipient in the macOS dialog."
			return p, p.request(Request{Version: Version, Operation: "call", ContactID: p.selected.ID, AddressID: a.ID, ExpectedValue: a.Value})
		}
	}
	return p, nil
}
func (p picker) View() string {
	var b strings.Builder
	b.WriteString("FaceTime audio — this Mac\n\n")
	if p.searching {
		fmt.Fprintf(&b, "Search: %s▏ (Enter to search)\n\n", Display(p.query))
	} else if p.query != "" {
		fmt.Fprintf(&b, "Search: %s\n\n", Display(p.query))
	}
	if p.busy {
		b.WriteString("Waiting for Contacts / FaceTime…\n")
	}
	if p.status != "" {
		b.WriteString(Display(p.status) + "\n\n")
	}
	if !p.busy {
		if p.selected != nil {
			fmt.Fprintf(&b, "%s — choose an address\n", Display(p.selected.Name))
			if len(p.selected.Addresses) == 0 {
				b.WriteString("No phone numbers or emails for this contact.\n")
			}
			for i, a := range p.selected.Addresses {
				mark := "  "
				if i == p.cursor {
					mark = "> "
				}
				fmt.Fprintf(&b, "%s%s: %s\n", mark, Display(a.Label), Display(a.Value))
			}
		} else {
			if len(p.contacts) == 0 {
				b.WriteString("No contacts found. Press r to retry or / to search.\n")
			}
			start := 0
			if p.cursor > 12 {
				start = p.cursor - 12
			}
			end := start + 15
			if end > len(p.contacts) {
				end = len(p.contacts)
			}
			for i := start; i < end; i++ {
				mark := "  "
				if i == p.cursor {
					mark = "> "
				}
				fmt.Fprintf(&b, "%s%s (%d addresses)\n", mark, Display(p.contacts[i].Name), len(p.contacts[i].Addresses))
			}
			if p.next != nil {
				b.WriteString("n: next page  ")
			}
			if p.offset > 0 {
				b.WriteString("p: previous page")
			}
		}
	}
	b.WriteString("\n↑/↓ select · Enter choose / call · / search · r refresh · Esc back / close\n")
	return b.String()
}
