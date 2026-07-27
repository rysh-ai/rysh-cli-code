package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

const todoBucket = "rysh-todos"

// TodoStatus represents the status of a todo item.
type TodoStatus string

const (
	TodoStatusPending    TodoStatus = "pending"
	TodoStatusInProgress TodoStatus = "in_progress"
	TodoStatusCompleted  TodoStatus = "completed"
)

// TodoItem is a single task entry.
type TodoItem struct {
	ID        string     `json:"id"`
	Text      string     `json:"text"`
	Status    TodoStatus `json:"status"`
	CreatedAt string     `json:"created_at"`
	UpdatedAt string     `json:"updated_at"`
}

// TodoList is the persisted todo state for a pane.
type TodoList struct {
	Items []TodoItem `json:"items"`
}

// TodoTool provides session-scoped task tracking backed by JetStream KV.
type TodoTool struct {
	js     nats.JetStreamContext
	paneID string
}

// TodoParams holds the parameters for todo operations.
type TodoParams struct {
	Action string `json:"action"`           // "list", "add", "update", "complete", "remove", "clear"
	ID     string `json:"id,omitempty"`     // item ID (for update, complete, remove)
	Text   string `json:"text,omitempty"`   // task text (for add)
	Status string `json:"status,omitempty"` // new status (for update: "pending", "in_progress", "completed")
}

// NewTodoTool creates a TodoTool for the given pane.
func NewTodoTool(js nats.JetStreamContext, paneID string) *TodoTool {
	return &TodoTool{js: js, paneID: paneID}
}

// Spec returns the tool specification for the LLM.
func (t *TodoTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["list", "add", "update", "complete", "remove", "clear"],
      "description": "The operation to perform"
    },
    "id": {"type": "string", "description": "Item ID (required for update, complete, remove)"},
    "text": {"type": "string", "description": "Task description (required for add)"},
    "status": {"type": "string", "enum": ["pending", "in_progress", "completed"], "description": "New status (for update action)"}
  },
  "required": ["action"]
}`)
	return ToolSpec{
		Name:             "todo",
		Description:      "Manage a session-scoped task list. Use frequently to track progress on multi-step tasks. Actions: list, add, update, complete, remove, clear.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false.
func (t *TodoTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute performs the todo operation.
func (t *TodoTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p TodoParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid todo params: %w", err)
	}

	if t.js == nil {
		return ErrOutput(ErrKindMissing, "JetStream not available"), nil
	}

	kv, err := t.getOrCreateBucket()
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to access todo store: %v", err), nil
	}

	switch p.Action {
	case "list":
		return t.list(kv)
	case "add":
		return t.add(kv, p.Text)
	case "update":
		return t.update(kv, p.ID, p.Status)
	case "complete":
		return t.complete(kv, p.ID)
	case "remove":
		return t.remove(kv, p.ID)
	case "clear":
		return t.clear(kv)
	default:
		return ErrOutputf(ErrKindValidation, "unknown action: %s", p.Action), nil
	}
}

func (t *TodoTool) kvKey() string {
	return t.paneID
}

func (t *TodoTool) loadList(kv nats.KeyValue) (*TodoList, error) {
	entry, err := kv.Get(t.kvKey())
	if err != nil {
		return &TodoList{}, nil // empty list if not found
	}
	var list TodoList
	if err := json.Unmarshal(entry.Value(), &list); err != nil {
		return &TodoList{}, nil
	}
	return &list, nil
}

func (t *TodoTool) saveList(kv nats.KeyValue, list *TodoList) error {
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	_, err = kv.Put(t.kvKey(), data)
	return err
}

func (t *TodoTool) list(kv nats.KeyValue) (*ToolOutput, error) {
	list, err := t.loadList(kv)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to load todos: %v", err), nil
	}
	if len(list.Items) == 0 {
		return &ToolOutput{Content: "(no tasks)"}, nil
	}

	var sb strings.Builder
	pending, inProgress, completed := 0, 0, 0
	for _, item := range list.Items {
		icon := "[ ]"
		switch item.Status {
		case TodoStatusInProgress:
			icon = "[~]"
			inProgress++
		case TodoStatusCompleted:
			icon = "[x]"
			completed++
		default:
			pending++
		}
		sb.WriteString(fmt.Sprintf("  %s %s  %s\n", icon, item.ID, item.Text))
	}
	sb.WriteString(fmt.Sprintf("\n(%d pending, %d in progress, %d completed)\n",
		pending, inProgress, completed))

	return &ToolOutput{Content: sb.String()}, nil
}

func (t *TodoTool) add(kv nats.KeyValue, text string) (*ToolOutput, error) {
	if text == "" {
		return ErrOutput(ErrKindValidation, "text is required for add action"), nil
	}

	list, err := t.loadList(kv)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to load todos: %v", err), nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	id := fmt.Sprintf("t%d", len(list.Items)+1)

	list.Items = append(list.Items, TodoItem{
		ID:        id,
		Text:      text,
		Status:    TodoStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	})

	if err := t.saveList(kv, list); err != nil {
		return ErrOutputf(ErrKindInternal, "failed to save: %v", err), nil
	}

	return &ToolOutput{
		Content: fmt.Sprintf("Added: [%s] %s", id, text),
		Metadata: map[string]string{
			"id": id,
		},
	}, nil
}

func (t *TodoTool) update(kv nats.KeyValue, id, status string) (*ToolOutput, error) {
	if id == "" {
		return ErrOutput(ErrKindValidation, "id is required for update action"), nil
	}
	if status == "" {
		return ErrOutput(ErrKindValidation, "status is required for update action"), nil
	}

	list, err := t.loadList(kv)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to load todos: %v", err), nil
	}

	for i, item := range list.Items {
		if item.ID == id {
			list.Items[i].Status = TodoStatus(status)
			list.Items[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if err := t.saveList(kv, list); err != nil {
				return ErrOutputf(ErrKindInternal, "failed to save: %v", err), nil
			}
			return &ToolOutput{Content: fmt.Sprintf("Updated: [%s] %s → %s", id, item.Text, status)}, nil
		}
	}

	return ErrOutputf(ErrKindMissing, "item %s not found", id), nil
}

func (t *TodoTool) complete(kv nats.KeyValue, id string) (*ToolOutput, error) {
	return t.update(kv, id, string(TodoStatusCompleted))
}

func (t *TodoTool) remove(kv nats.KeyValue, id string) (*ToolOutput, error) {
	if id == "" {
		return ErrOutput(ErrKindValidation, "id is required for remove action"), nil
	}

	list, err := t.loadList(kv)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to load todos: %v", err), nil
	}

	for i, item := range list.Items {
		if item.ID == id {
			list.Items = append(list.Items[:i], list.Items[i+1:]...)
			if err := t.saveList(kv, list); err != nil {
				return ErrOutputf(ErrKindInternal, "failed to save: %v", err), nil
			}
			return &ToolOutput{Content: fmt.Sprintf("Removed: [%s] %s", id, item.Text)}, nil
		}
	}

	return ErrOutputf(ErrKindMissing, "item %s not found", id), nil
}

func (t *TodoTool) clear(kv nats.KeyValue) (*ToolOutput, error) {
	list, _ := t.loadList(kv)
	count := len(list.Items)

	if err := kv.Delete(t.kvKey()); err != nil {
		// Ignore "key not found" errors.
		_ = err
	}

	return &ToolOutput{Content: fmt.Sprintf("Cleared %d items.", count)}, nil
}

func (t *TodoTool) getOrCreateBucket() (nats.KeyValue, error) {
	kv, err := t.js.KeyValue(todoBucket)
	if err == nil {
		return kv, nil
	}
	return t.js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:  todoBucket,
		Storage: nats.FileStorage,
	})
}
