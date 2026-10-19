// SPDX-License-Identifier: Apache-2.0

package toolpackgen

import (
	"github.com/rysh-ai/rysh-cli-shared/forge/ir"
	"github.com/rysh-ai/rysh-cli-shared/forge/toolpack"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/gen"
)

// Package toolpackgen is the "rysh-toolpack" generator target. The toolpack
// itself lives in rysh-shared (forge/toolpack) so rysh-server can register the
// same tools inside a customer chatbot; only this generator registration, which
// depends on the CLI-side gen registry, stays here.
//
// init registers the "rysh-toolpack" generator target, which emits the
// declarative manifest as a JSON artifact. The same manifest is what the live
// loader (##integration enable) consumes to register tools into the registry.
func init() { gen.Register(&manifestGenerator{}) }

type manifestGenerator struct{}

func (g *manifestGenerator) Name() string { return "rysh-toolpack" }

func (g *manifestGenerator) Generate(api *ir.API, opts gen.Options) (*gen.FileSet, error) {
	tp := toolpack.Build(api, opts.Prefix)
	data, err := tp.JSON()
	if err != nil {
		return nil, err
	}
	fs := gen.NewFileSet()
	fs.Add("toolpack.json", data)
	return fs, nil
}
