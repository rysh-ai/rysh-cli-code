// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// memBlobs is an in-memory imageBlobStore for testing the transforms without a
// live JetStream Object Store.
type memBlobs struct {
	m       map[string][]byte
	putErr  bool
	getMiss bool
}

func newMemBlobs() *memBlobs { return &memBlobs{m: map[string][]byte{}} }

func (b *memBlobs) put(key string, data []byte) error {
	if b.putErr {
		return errors.New("put failed")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	b.m[key] = cp
	return nil
}

func (b *memBlobs) get(key string) ([]byte, error) {
	if b.getMiss {
		return nil, errors.New("not found")
	}
	d, ok := b.m[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return d, nil
}

func convJSON(t *testing.T, turns []provider.ConversationTurn) []byte {
	t.Helper()
	b, err := json.Marshal(turns)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func imageTurn(mediaType, b64 string) provider.ConversationTurn {
	return provider.ConversationTurn{
		Role: "user",
		ContentBlocks: []provider.ContentBlock{
			provider.ContentBlockText("look at this"),
			provider.ContentBlockImageBase64(mediaType, b64),
		},
	}
}

func TestOffloadInline_RoundTrip(t *testing.T) {
	blobs := newMemBlobs()
	rawImg := []byte("\x89PNG\r\n\x1a\nfake-image-bytes-larger-than-a-name")
	b64 := base64.StdEncoding.EncodeToString(rawImg)

	original := convJSON(t, []provider.ConversationTurn{imageTurn("image/png", b64)})

	// Offload: the persisted value must NOT contain the base64 data.
	slimmed, ok := offloadConversationImages(original, blobs)
	if !ok {
		t.Fatal("expected offload to report a change")
	}
	if strings.Contains(string(slimmed), b64) {
		t.Fatal("slimmed conversation still contains inline base64")
	}
	if len(blobs.m) != 1 {
		t.Fatalf("expected exactly one object stored, got %d", len(blobs.m))
	}
	// The slimmed value must carry a file_id ref.
	var turns []provider.ConversationTurn
	if err := json.Unmarshal(slimmed, &turns); err != nil {
		t.Fatal(err)
	}
	src := turns[0].ContentBlocks[1].Source
	if src.Type != "file_id" || src.FileID == "" || src.Data != "" {
		t.Fatalf("expected a file_id ref with no inline data, got %+v", src)
	}
	if src.MediaType != "image/png" {
		t.Fatalf("media type not preserved: %q", src.MediaType)
	}

	// Inline: rehydrate back to the original base64.
	inlined, ok := inlineConversationImages(slimmed, blobs)
	if !ok {
		t.Fatal("expected inline to report a change")
	}
	var back []provider.ConversationTurn
	if err := json.Unmarshal(inlined, &back); err != nil {
		t.Fatal(err)
	}
	got := back[0].ContentBlocks[1].Source
	if got.Type != "base64" || got.Data != b64 || got.MediaType != "image/png" {
		t.Fatalf("round-trip lost image data: %+v", got)
	}
}

func TestOffload_Dedup(t *testing.T) {
	blobs := newMemBlobs()
	b64 := base64.StdEncoding.EncodeToString([]byte("same-image-bytes"))
	// Two turns with the same image.
	turns := []provider.ConversationTurn{imageTurn("image/png", b64), imageTurn("image/png", b64)}
	if _, ok := offloadConversationImages(convJSON(t, turns), blobs); !ok {
		t.Fatal("expected offload change")
	}
	if len(blobs.m) != 1 {
		t.Fatalf("identical images should dedup to one object, got %d", len(blobs.m))
	}
}

func TestOffload_NoImagesNoChange(t *testing.T) {
	blobs := newMemBlobs()
	turns := []provider.ConversationTurn{{Role: "user", Content: "just text"}}
	if _, ok := offloadConversationImages(convJSON(t, turns), blobs); ok {
		t.Fatal("text-only conversation should not be changed")
	}
}

func TestOffload_PutFailureLeavesInline(t *testing.T) {
	blobs := newMemBlobs()
	blobs.putErr = true
	b64 := base64.StdEncoding.EncodeToString([]byte("img"))
	_, ok := offloadConversationImages(convJSON(t, []provider.ConversationTurn{imageTurn("image/png", b64)}), blobs)
	if ok {
		t.Fatal("a failed upload should leave the block inline (no change reported)")
	}
}

func TestInline_MissingObjectDegrades(t *testing.T) {
	blobs := newMemBlobs()
	blobs.getMiss = true
	// A conversation already in ref form.
	turns := []provider.ConversationTurn{{
		Role: "user",
		ContentBlocks: []provider.ContentBlock{
			{Type: "image", Source: &provider.ImageSource{Type: "file_id", MediaType: "image/png", FileID: "img-deadbeef"}},
		},
	}}
	inlined, ok := inlineConversationImages(convJSON(t, turns), blobs)
	if !ok {
		t.Fatal("expected a change (degrade to placeholder)")
	}
	var back []provider.ConversationTurn
	if err := json.Unmarshal(inlined, &back); err != nil {
		t.Fatal(err)
	}
	blk := back[0].ContentBlocks[0]
	if blk.Type != "text" || !strings.Contains(blk.Text, "unavailable") {
		t.Fatalf("missing object should degrade to a text placeholder, got %+v", blk)
	}
}

func TestNewImageOffloadKV_NilObjStoreFallsBack(t *testing.T) {
	if got := newImageOffloadKV(nil, nil); got != nil {
		t.Fatal("nil kv + nil obj should return nil (caller keeps raw kv)")
	}
}
