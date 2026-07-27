package bus

// JetStream Object Store image persistence (follow-up 1b).
//
// Conversation values (the {paneID}.llm_conversation key) are a JSON
// []provider.ConversationTurn that may carry inline base64 images attached via
// `##image`. A single >~768 KB image base64-encodes past the JetStream
// per-message size limit (default 1 MB), so the conversation Put fails — and
// because LLMPromptExecutionActor.persistConversation ignores the Put error,
// the conversation is silently dropped on the next restart.
//
// imageOffloadKV wraps the pane KV and transparently moves inline base64 image
// blocks into a JetStream Object Store on Put (replacing them with a content-
// hash ref), rehydrating them on Get. The conversation value that lands in KV
// stays small; the rest of the system (LLM actor, provider) is unchanged
// because it only ever sees fully-inlined base64 turns. All non-conversation
// keys pass straight through — nats.KeyValue is an interface, so embedding
// forwards every other method unchanged.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// conversationKeySuffix marks the only KV key whose value is transformed.
const conversationKeySuffix = ".llm_conversation"

// imageBlobStore is the minimal blob interface the transforms need; it keeps
// them unit-testable without a live JetStream Object Store.
type imageBlobStore interface {
	put(key string, data []byte) error // idempotent (content-addressed)
	get(key string) ([]byte, error)
}

// natsBlobStore adapts a nats.ObjectStore to imageBlobStore with content-hash
// dedup (an object already present is not re-uploaded).
type natsBlobStore struct{ obj nats.ObjectStore }

func (n natsBlobStore) put(key string, data []byte) error {
	if _, err := n.obj.GetInfo(key); err == nil {
		return nil // already stored (content-addressed dedup)
	}
	_, err := n.obj.PutBytes(key, data)
	return err
}

func (n natsBlobStore) get(key string) ([]byte, error) { return n.obj.GetBytes(key) }

// imageOffloadKV is the pane KV wrapper.
type imageOffloadKV struct {
	nats.KeyValue
	blobs imageBlobStore
}

// newImageOffloadKV wraps kv to offload conversation images into obj. Returns
// the raw kv unchanged when obj is nil (graceful fallback to inline base64).
func newImageOffloadKV(kv nats.KeyValue, obj nats.ObjectStore) nats.KeyValue {
	if obj == nil {
		return kv
	}
	return &imageOffloadKV{KeyValue: kv, blobs: natsBlobStore{obj: obj}}
}

func (k *imageOffloadKV) Put(key string, value []byte) (uint64, error) {
	if strings.HasSuffix(key, conversationKeySuffix) {
		if slimmed, ok := offloadConversationImages(value, k.blobs); ok {
			value = slimmed
		}
	}
	return k.KeyValue.Put(key, value)
}

func (k *imageOffloadKV) Get(key string) (nats.KeyValueEntry, error) {
	entry, err := k.KeyValue.Get(key)
	if err != nil || entry == nil {
		return entry, err
	}
	if strings.HasSuffix(key, conversationKeySuffix) {
		if inlined, ok := inlineConversationImages(entry.Value(), k.blobs); ok {
			return rewrittenEntry{KeyValueEntry: entry, value: inlined}, nil
		}
	}
	return entry, err
}

// rewrittenEntry overrides Value() with the rehydrated bytes; key/revision/
// timestamps forward to the embedded entry.
type rewrittenEntry struct {
	nats.KeyValueEntry
	value []byte
}

func (e rewrittenEntry) Value() []byte { return e.value }

// imageObjectKey is the content-addressed Object Store key for raw image bytes.
func imageObjectKey(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "img-" + hex.EncodeToString(sum[:])
}

// offloadConversationImages swaps every inline base64 image block for an
// Object Store ref (Source.Type "file_id", FileID = content hash). Returns
// (slimmedJSON, true) when at least one block was offloaded. A block whose
// upload fails is left inline (best-effort), so persistence still attempts.
// Returns (nil, false) when the value isn't a conversation or has no inline
// images to offload.
func offloadConversationImages(value []byte, blobs imageBlobStore) ([]byte, bool) {
	var turns []provider.ConversationTurn
	if err := json.Unmarshal(value, &turns); err != nil {
		return nil, false
	}
	changed := false
	for ti := range turns {
		blocks := turns[ti].ContentBlocks
		for bi := range blocks {
			src := blocks[bi].Source
			if blocks[bi].Type != "image" || src == nil || src.Type != "base64" || src.Data == "" {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(src.Data)
			if err != nil {
				continue
			}
			key := imageObjectKey(raw)
			if err := blobs.put(key, raw); err != nil {
				continue // best-effort: leave this block inline
			}
			blocks[bi].Source = &provider.ImageSource{
				Type:      "file_id",
				MediaType: src.MediaType,
				FileID:    key,
			}
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(turns)
	if err != nil {
		return nil, false
	}
	return out, true
}

// inlineConversationImages is the inverse of offloadConversationImages: it
// rehydrates every Object Store ref back into an inline base64 block so the
// rest of the system only ever sees inline images. An object that can't be
// fetched (e.g. store wiped) degrades to a "[image unavailable]" text block so
// the conversation still loads. Returns (inlinedJSON, true) when at least one
// ref was rehydrated.
func inlineConversationImages(value []byte, blobs imageBlobStore) ([]byte, bool) {
	var turns []provider.ConversationTurn
	if err := json.Unmarshal(value, &turns); err != nil {
		return nil, false
	}
	changed := false
	for ti := range turns {
		blocks := turns[ti].ContentBlocks
		for bi := range blocks {
			src := blocks[bi].Source
			if blocks[bi].Type != "image" || src == nil || src.Type != "file_id" || src.FileID == "" {
				continue
			}
			raw, err := blobs.get(src.FileID)
			if err != nil {
				blocks[bi] = provider.ContentBlockText("[image unavailable]")
				changed = true
				continue
			}
			blocks[bi].Source = &provider.ImageSource{
				Type:      "base64",
				MediaType: src.MediaType,
				Data:      base64.StdEncoding.EncodeToString(raw),
			}
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(turns)
	if err != nil {
		return nil, false
	}
	return out, true
}
