package main

import (
	"os"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/forgecmd"
)

// runForgeCommand implements the `rysh forge` subcommand: turn an API spec into
// a tool-pack, SDKs, an MCP server, and docs, and register it for live use.
// The actual logic lives in internal/forge/forgecmd so it can be shared with the
// in-session `##forge` command; here we just bind it to the process CWD + stdout.
//
//	rysh forge add <name> <spec-file> [flags]   ingest a spec + generate artifacts
//	rysh forge add <name> --graphql-url URL     introspect a running GraphQL endpoint
//	rysh forge add <name> --grpc-target H:P     discover a running gRPC server (reflection)
//	rysh forge generate <name> [flags]          re-generate from the stored spec
//	rysh forge list                             list configured integrations
//	rysh forge diff <name> <new-spec-file>      operation-level changes vs the stored spec
//	rysh forge targets                          list available generator targets
func runForgeCommand(args []string) error {
	workDir, _ := os.Getwd()
	return forgecmd.Run(workDir, args, os.Stdout)
}
