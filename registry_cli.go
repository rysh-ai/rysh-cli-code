package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/registry"
)

// resolveRyshDir returns the target .rysh directory for package installs.
func resolveRyshDir(cfg config.Config) string {
	if strings.TrimSpace(cfg.RyshDir) != "" {
		return cfg.RyshDir
	}
	return filepath.Join(".", ".rysh")
}

// runRegistryInstall implements `rysh install <dir|tarball|url> [--yes] [--force]`.
// (design 005) Installs an agent/loop/recipe/humanoid package into .rysh/,
// showing declared tools/channels/env before writing, and records it in
// .rysh/rysh.lock for reproducibility.
func runRegistryInstall(cfg config.Config, args []string) error {
	var spec string
	var opts registry.Options
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			opts.Yes = true
		case "--force", "-f":
			opts.Force = true
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("install: unknown flag %q", a)
			}
			if spec == "" {
				spec = a
			}
		}
	}
	if spec == "" {
		return errors.New("usage: rysh install <@ns/name[@version]|dir|tarball|url> [--yes] [--force]")
	}

	ryshDir := resolveRyshDir(cfg)

	// `@ns/name` goes through the registry index. Handled before the
	// dir/tarball branches because a namespaced spec is neither.
	if registry.IsNamespaced(spec) {
		return installFromRegistry(cfg, spec, ryshDir, opts)
	}

	// Peek at the manifest to show consent before writing, when it's a dir.
	if info, err := os.Stat(spec); err == nil && info.IsDir() {
		if data, rerr := os.ReadFile(filepath.Join(spec, registry.ManifestFile)); rerr == nil {
			if m, perr := registry.ParseManifest(data); perr == nil {
				fmt.Print(m.ConsentSummary())
			}
		}
	}

	var (
		m        *registry.Manifest
		warnings []string
		err      error
	)
	isTar := strings.HasSuffix(spec, ".tar.gz") || strings.HasSuffix(spec, ".tgz") ||
		strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://")
	if isTar {
		m, warnings, err = registry.InstallFromTarball(spec, ryshDir, opts)
	} else {
		m, warnings, err = registry.InstallFromDir(spec, ryshDir, opts)
	}
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Printf("warning: %s\n", w)
	}
	td, _ := registry.TypeDir(m.Type)
	fmt.Printf("installed %s@%s → %s\n", m.Name, m.Version,
		filepath.Join(ryshDir, td, registry.InstallName(m.Name)))
	if m.Type == "agent" {
		fmt.Printf("spawn it with:  ##agent spawn %s\n", registry.InstallName(m.Name))
	}
	return nil
}

// runRegistryList implements `rysh list-packages`: show installed packages.
func runRegistryList(cfg config.Config) error {
	lock, err := registry.LoadLock(resolveRyshDir(cfg))
	if err != nil {
		return err
	}
	names := lock.InstalledNames()
	if len(names) == 0 {
		fmt.Println("no packages installed (see rysh install)")
		return nil
	}
	fmt.Printf("Installed packages (%d):\n", len(names))
	for _, n := range names {
		e := lock.Packages[n]
		fmt.Printf("  %-32s %-10s %s\n", n, e.Version, e.Type)
	}
	return nil
}

// installFromRegistry resolves `@ns/name[@version]` through the index and
// installs the artifact it points at.
//
// The consent summary is printed from the manifest INSIDE the verified archive,
// not from the index: the index says where the bytes are, the package says what
// it wants. Trusting the index for the latter would let a compromised catalogue
// under-declare a package's tools and env access.
func installFromRegistry(cfg config.Config, spec, ryshDir string, opts registry.Options) error {
	indexURL := registryIndexURL(cfg)

	idx, err := registry.FetchIndex(indexURL, 0)
	if err != nil {
		return err
	}
	name, version, art, err := idx.Resolve(spec)
	if err != nil {
		return err
	}
	artifactURL := registry.AbsoluteURL(indexURL, art.URL)
	fmt.Printf("resolved %s -> %s@%s\n  %s\n", spec, name, version, artifactURL)

	m, warnings, err := registry.InstallFromIndex(artifactURL, art.Checksum, ryshDir, opts)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Printf("warning: %s\n", w)
	}
	td, _ := registry.TypeDir(m.Type)
	fmt.Printf("installed %s@%s -> %s\n", m.Name, m.Version,
		filepath.Join(ryshDir, td, registry.InstallName(m.Name)))
	if m.Type == "agent" {
		fmt.Printf("spawn it with:  ##agent spawn %s\n", registry.InstallName(m.Name))
	}
	return nil
}

// registryIndexURL resolves the index location: RYSH_REGISTRY_URL beats config,
// config beats the first-party default. The env override is what makes an
// air-gapped or company-internal registry a one-liner rather than a config edit.
func registryIndexURL(cfg config.Config) string {
	if v := strings.TrimSpace(os.Getenv("RYSH_REGISTRY_URL")); v != "" {
		return v
	}
	if v := strings.TrimSpace(cfg.Registry.URL); v != "" {
		return v
	}
	return registry.DefaultIndexURL
}

// runRegistrySearch implements `rysh search <query>`: a client-side scan of
// the fetched static index (design 005 keeps the read path serviceless).
func runRegistrySearch(cfg config.Config, args []string) error {
	query := ""
	if len(args) > 0 {
		query = strings.Join(args, " ")
	}
	indexURL := registryIndexURL(cfg)
	idx, err := registry.FetchIndex(indexURL, 0)
	if err != nil {
		return err
	}
	results := idx.Search(query)
	if len(results) == 0 {
		fmt.Printf("no packages matching %q (index: %s)\n", query, indexURL)
		return nil
	}
	fmt.Printf("%-32s %-10s %-9s %s\n", "PACKAGE", "LATEST", "TYPE", "DESCRIPTION")
	for _, r := range results {
		fmt.Printf("%-32s %-10s %-9s %s\n", r.Name, r.Latest, r.Type, r.Description)
	}
	return nil
}

// runRegistryUpdate implements `rysh update [@ns/name] [--yes] [--force]`:
// reinstall any namespaced package the index publishes a newer version of.
// Without a name it updates everything outdated; with one it updates that
// package (erroring if it is not installed).
func runRegistryUpdate(cfg config.Config, args []string) error {
	var target string
	var opts registry.Options
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			opts.Yes = true
		case "--force", "-f":
			opts.Force = true
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("update: unknown flag %q", a)
			}
			if target == "" {
				target = a
			}
		}
	}

	ryshDir := resolveRyshDir(cfg)
	lock, err := registry.LoadLock(ryshDir)
	if err != nil {
		return err
	}
	if target != "" {
		if _, ok := lock.Packages[target]; !ok {
			return fmt.Errorf("update: %s is not installed (see rysh list-packages)", target)
		}
	}

	indexURL := registryIndexURL(cfg)
	idx, err := registry.FetchIndex(indexURL, 0)
	if err != nil {
		return err
	}

	candidates := idx.Outdated(lock)
	if target != "" {
		filtered := candidates[:0]
		for _, c := range candidates {
			if c.Name == target {
				filtered = append(filtered, c)
			}
		}
		candidates = filtered
	}
	if len(candidates) == 0 {
		if target != "" {
			fmt.Printf("%s is up to date (%s)\n", target, lock.Packages[target].Version)
		} else {
			fmt.Println("all packages up to date")
		}
		return nil
	}

	for _, c := range candidates {
		fmt.Printf("updating %s %s -> %s\n", c.Name, c.Installed, c.Latest)
		artifactURL := registry.AbsoluteURL(indexURL, c.Artifact.URL)
		m, warnings, err := registry.InstallFromIndex(artifactURL, c.Artifact.Checksum, ryshDir, opts)
		if err != nil {
			return fmt.Errorf("update %s: %w", c.Name, err)
		}
		for _, w := range warnings {
			fmt.Printf("warning: %s\n", w)
		}
		fmt.Printf("updated %s -> %s\n", m.Name, m.Version)
	}
	return nil
}
