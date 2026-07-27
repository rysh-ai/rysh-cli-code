package ingest

import (
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// LooksLikeDescriptorSet reports whether data is plausibly a compiled protobuf
// FileDescriptorSet, for source auto-detection when the filename gives no hint.
//
// Deliberately conservative. proto.Unmarshal is lenient — it will happily
// consume many byte strings, including some JSON — so a bare "did it parse?"
// test would misclassify OpenAPI and GraphQL specs as gRPC. Three gates:
//
//  1. Reject valid UTF-8 text outright. Every OpenAPI/GraphQL spec rysh accepts
//     is text; a real descriptor set contains length-prefixed binary fields and
//     is vanishingly unlikely to be valid UTF-8 in full.
//  2. Require the payload to parse as a FileDescriptorSet with at least one
//     file.
//  3. Require that file to carry a name, which every protoc/buf output does.
//     Random bytes that happen to parse will not satisfy this.
//
// False negatives are cheap — the user passes --source grpc. A false positive
// would send an OpenAPI spec down the gRPC path, so the bias is intentional.
func LooksLikeDescriptorSet(data []byte) bool {
	if len(data) == 0 || utf8.Valid(data) {
		return false
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &fds); err != nil {
		return false
	}
	files := fds.GetFile()
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if f.GetName() != "" {
			return true
		}
	}
	return false
}
