// SPDX-License-Identifier: Apache-2.0

package forgecmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// feedDescriptorSet builds a FileDescriptorSet for service Feed with one unary
// method (GetItem) plus one server-streaming method per name in streams.
// Varying only `streams` between two calls yields two specs that differ solely
// in their stream sets.
func feedDescriptorSet(t *testing.T, streams ...string) []byte {
	t.Helper()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL

	item := &descriptorpb.DescriptorProto{
		Name: proto.String("Item"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("text"), Number: proto.Int32(1), Label: &opt, Type: &str},
		},
	}
	methods := []*descriptorpb.MethodDescriptorProto{{
		Name:       proto.String("GetItem"),
		InputType:  proto.String(".feed.Item"),
		OutputType: proto.String(".feed.Item"),
	}}
	for _, name := range streams {
		methods = append(methods, &descriptorpb.MethodDescriptorProto{
			Name:            proto.String(name),
			InputType:       proto.String(".feed.Item"),
			OutputType:      proto.String(".feed.Item"),
			ServerStreaming: proto.Bool(true),
		})
	}
	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{{
			Name:        proto.String("feed.proto"),
			Package:     proto.String("feed"),
			Syntax:      proto.String("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{item},
			Service: []*descriptorpb.ServiceDescriptorProto{{
				Name:   proto.String("Feed"),
				Method: methods,
			}},
		}},
	}
	b, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	return b
}

// TestForgeDiff_ShowsStreamChanges is the user-facing half of T5b. DiffAPIs
// learning about streams is worth nothing if `forge diff` — the command a
// person actually runs before adopting a new spec — does not print them.
//
// The two specs here differ ONLY in their server-streaming methods, so before
// T5b this command printed "No operation-level changes" and a user would have
// adopted a spec that dropped a live streaming session without being told.
func TestForgeDiff_ShowsStreamChanges(t *testing.T) {
	workDir := t.TempDir()
	oldSpec := filepath.Join(workDir, "feed-old.pb")
	newSpec := filepath.Join(workDir, "feed-new.pb")
	if err := os.WriteFile(oldSpec, feedDescriptorSet(t, "Watch", "Tail"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newSpec, feedDescriptorSet(t, "Watch", "Follow"), 0o644); err != nil {
		t.Fatal(err)
	}

	var addOut bytes.Buffer
	if err := Run(workDir, []string{
		"add", "feed", "feed-old.pb",
		"--base-url", "http://127.0.0.1:8080",
		"--targets", "docs",
	}, &addOut); err != nil {
		t.Fatalf("forge add: %v\noutput:\n%s", err, addOut.String())
	}

	var out bytes.Buffer
	if err := Run(workDir, []string{"diff", "feed", "feed-new.pb"}, &out); err != nil {
		t.Fatalf("forge diff: %v\noutput:\n%s", err, out.String())
	}
	got := out.String()

	if strings.Contains(got, "No changes") {
		t.Fatalf("forge diff reported no changes, but Feed_Tail was removed and Feed_Follow added:\n%s", got)
	}
	if !strings.Contains(got, "Stream changes") {
		t.Errorf("diff output has no stream section:\n%s", got)
	}
	if !strings.Contains(got, "+ Feed_Follow") {
		t.Errorf("diff output does not report the added stream Feed_Follow:\n%s", got)
	}
	if !strings.Contains(got, "- Feed_Tail") {
		t.Errorf("diff output does not report the removed stream Feed_Tail:\n%s", got)
	}
	// The unary operation is untouched, so no operation section should appear.
	if strings.Contains(got, "Operation changes") {
		t.Errorf("diff output claims operation changes, but only streams differ:\n%s", got)
	}
}

// TestForgeDiff_IdenticalSpecReportsNoChanges keeps the other direction honest:
// widening the diff must not make every comparison look dirty.
func TestForgeDiff_IdenticalSpecReportsNoChanges(t *testing.T) {
	workDir := t.TempDir()
	spec := feedDescriptorSet(t, "Watch", "Tail")
	if err := os.WriteFile(filepath.Join(workDir, "feed.pb"), spec, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "feed-copy.pb"), spec, 0o644); err != nil {
		t.Fatal(err)
	}

	var addOut bytes.Buffer
	if err := Run(workDir, []string{
		"add", "feed", "feed.pb",
		"--base-url", "http://127.0.0.1:8080",
		"--targets", "docs",
	}, &addOut); err != nil {
		t.Fatalf("forge add: %v\noutput:\n%s", err, addOut.String())
	}

	var out bytes.Buffer
	if err := Run(workDir, []string{"diff", "feed", "feed-copy.pb"}, &out); err != nil {
		t.Fatalf("forge diff: %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "No changes") {
		t.Errorf("identical specs should report no changes, got:\n%s", out.String())
	}
}
