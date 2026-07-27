package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// EmailAttachTool adds a file attachment to an existing email draft.
type EmailAttachTool struct {
	drafts *channels.DraftStore
}

// EmailAttachParams holds the parameters for the email_attach tool.
type EmailAttachParams struct {
	DraftID  string `json:"draft_id"`
	FilePath string `json:"file_path"`
}

// NewEmailAttachTool creates a new EmailAttachTool.
func NewEmailAttachTool(drafts *channels.DraftStore) *EmailAttachTool {
	return &EmailAttachTool{drafts: drafts}
}

// Spec returns the tool specification.
func (t *EmailAttachTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "draft_id": {"type": "string", "description": "ID of the draft to attach the file to"},
    "file_path": {"type": "string", "description": "Absolute path to the file to attach"}
  },
  "required": ["draft_id", "file_path"]
}`)
	return ToolSpec{
		Name:             "email_attach",
		Description:      "Add a file attachment to an existing email draft. The file is read and embedded in the draft.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval returns false — attaching a file does not send anything.
func (t *EmailAttachTool) RequiresApproval(_ json.RawMessage) bool { return false }

// Execute reads a file and attaches it to an existing draft.
func (t *EmailAttachTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p EmailAttachParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid email_attach params: %w", err)
	}
	if p.DraftID == "" || p.FilePath == "" {
		return ErrOutput(ErrKindValidation, "draft_id and file_path are required"), nil
	}

	data, err := os.ReadFile(p.FilePath)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "read file failed: %v", err), nil
	}

	filename := filepath.Base(p.FilePath)
	contentType := mime.TypeByExtension(filepath.Ext(p.FilePath))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	att := channels.Attachment{
		Filename:    filename,
		ContentType: contentType,
		Data:        data,
	}

	if err := t.drafts.AddAttachment(p.DraftID, att); err != nil {
		return ErrOutput(ErrKindInternal, err.Error()), nil
	}

	return &ToolOutput{
		Content: fmt.Sprintf("Attached %q (%s, %d bytes) to %s.", filename, contentType, len(data), p.DraftID),
		Metadata: map[string]string{
			"filename": filename,
			"size":     fmt.Sprintf("%d", len(data)),
		},
	}, nil
}
