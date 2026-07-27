package toolpack

import (
	"github.com/rysh-ai/rysh-cli-code/internal/forge/gen"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

// init registers the "rysh-toolpack" generator target, which emits the
// declarative manifest as a JSON artifact. The same manifest is what the live
// loader (##integration enable) consumes to register tools into the registry.
func init() { gen.Register(&manifestGenerator{}) }

type manifestGenerator struct{}

func (g *manifestGenerator) Name() string { return "rysh-toolpack" }

func (g *manifestGenerator) Generate(api *ir.API, opts gen.Options) (*gen.FileSet, error) {
	tp := Build(api, opts.Prefix)
	data, err := tp.JSON()
	if err != nil {
		return nil, err
	}
	fs := gen.NewFileSet()
	fs.Add("toolpack.json", data)
	return fs, nil
}
