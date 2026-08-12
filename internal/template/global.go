package template

// The host-wide defaults layer: one clawk.mod outside any repo whose sandbox
// block supplies values for every sandbox created on this machine. It answers
// the "same file over and over" problem (clawkwork/clawk#14) — a kernel path, a
// token alias, a personal skill mount and a house rule are properties of the
// HOST, not of the repo they currently sit in.
//
// It is the lowest layer of the precedence chain:
//
//	built-in defaults
//	  < ~/.config/clawk/clawk.mod   (this file)
//	  < namespace
//	  < repo clawk.mod
//	  < clawk.mod.<profile>
//	  < flags
//
// Lists union with the global entries first, so a conflict message reads
// scope-outward; scalars are filled only where nothing narrower declared one.
// It is read at sandbox-create time like every other template, so editing it
// never retro-modifies existing sandboxes.

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// GlobalModEnvVar names the environment variable that overrides the host-wide
// defaults file outright. Pointing it at a file makes a run reproducible
// regardless of what the host happens to have in ~/.config; pointing it at a
// path that does not exist is an error rather than a silent fall-through, so a
// typo in CI surfaces.
//
// Named for the layer it overrides, so it pairs with --no-global and can't be
// misread as "the repo's clawk.mod", and prefixed like every other variable
// clawk reads (CLAWK_DEBUG, CLAWK_NET_MODE, CLAWK_MAX_VZ_DEVICES) so it
// doesn't collide with an unrelated ROOT_* in someone's shell.
const GlobalModEnvVar = "CLAWK_GLOBAL_MOD"

// GlobalDisabled skips the host-wide layer entirely — wired to `--no-global`.
// A package var rather than a parameter threaded through nine loader
// signatures: it is written once from PersistentPreRunE before any load, and
// tests set and restore it around a call.
var GlobalDisabled bool

// ErrNoGlobalMod reports that no host-wide defaults file exists. Not a
// failure — the overwhelmingly common case is a host that never wrote one.
var ErrNoGlobalMod = errors.New("no host-wide clawk.mod")

// ErrGlobalMod marks every OTHER host-wide-layer failure: unreadable,
// unparseable, out-of-scope directive, two candidate locations. Callers that
// try loaders in sequence (see the cli's resolveSource, which walks workspace →
// standalone → bare-git-repo and treats a failure as "not this shape") must
// test for it and surface it instead of moving on: a broken defaults file is
// not a hint to try somewhere else, and degrading to "no defaults" would hand
// back a sandbox quietly missing half its configuration.
var ErrGlobalMod = errors.New("host-wide clawk.mod")

// GlobalModPath resolves the host-wide defaults file, in order:
//
//	$CLAWK_GLOBAL_MOD                        explicit override (must exist)
//	$XDG_CONFIG_HOME/clawk/clawk.mod         default ~/.config/clawk/clawk.mod
//	~/.clawk/clawk.mod                       compatibility fallback
//
// ~/.config is the primary home because this file is the one thing in clawk's
// footprint a user hand-edits, symlinks out of a dotfiles repo and would be
// annoyed to lose: ~/.clawk is disposable machine state (VM disks, an image
// cache, per-sandbox records, a live OAuth token) that people exclude from
// backups and delete to start clean. Config must not be collateral.
//
// Deliberately NOT os.UserConfigDir(): on darwin that is
// ~/Library/Application Support, which is the wrong place for a
// dotfile-managed text file. Honouring $XDG_CONFIG_HOME with a ~/.config
// fallback on every platform is what gh, git and nvim do on macOS, and what
// anyone writing this file expects.
//
// Both non-env locations present is an error, never a silent precedence pick.
func GlobalModPath() (string, error) {
	if p := os.Getenv(GlobalModEnvVar); p != "" {
		expanded, err := ExpandPath(p)
		if err != nil {
			return "", err
		}
		abs, err := filepath.Abs(expanded)
		if err != nil {
			return "", err
		}
		if !fileExists(abs) {
			return "", fmt.Errorf("%s=%s: no such file", GlobalModEnvVar, p)
		}
		return abs, nil
	}

	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", ErrNoGlobalMod
		}
		xdg = filepath.Join(home, ".config")
	}
	primary := filepath.Join(xdg, "clawk", RepoFileName)

	var legacy string
	if home, err := os.UserHomeDir(); err == nil {
		legacy = filepath.Join(home, ".clawk", RepoFileName)
	}

	switch {
	case fileExists(primary) && legacy != "" && fileExists(legacy):
		return "", fmt.Errorf(
			"two host-wide clawk.mod files: %s and %s — keep one (%s is the documented location) or set %s",
			primary, legacy, primary, GlobalModEnvVar)
	case fileExists(primary):
		return primary, nil
	case legacy != "" && fileExists(legacy):
		return legacy, nil
	}
	return "", ErrNoGlobalMod
}

// Global is a loaded host-wide defaults layer.
type Global struct {
	// Path is the file it came from, for the note printed at create.
	Path string
	// Template is the file's sandbox block with every host-side path made
	// absolute against Path's directory (see absolutiseHostPaths), so it can
	// be merged under a repo template that resolves paths against its own
	// root.
	Template *Template
	// Policies are `policy <name> ( … )` blocks declared beside it — a
	// personal policy library, registered by the create paths exactly like a
	// repo's own.
	Policies []PolicyDef
	// ProfileMatched reports whether a clawk.mod.<profile> overlay beside the
	// global file existed and was applied, so a profile satisfied only by the
	// host-wide layer is not reported as matching nothing.
	ProfileMatched bool
}

// LoadGlobal is LoadGlobalWithProfile with no profile.
func LoadGlobal() (*Global, error) { return LoadGlobalWithProfile("") }

// LoadGlobalWithProfile loads and validates the host-wide defaults layer,
// applying a clawk.mod.<profile> overlay beside it when profile is non-empty.
// Returns ErrNoGlobalMod when there is no such file (or --no-global was
// passed) — callers treat that as "no defaults", not as a failure.
func LoadGlobalWithProfile(profile string) (*Global, error) {
	g, err := loadGlobal(profile)
	if err != nil && !errors.Is(err, ErrNoGlobalMod) {
		// Tagged so a loader ladder can tell "this layer is broken" from
		// "this shape doesn't apply here" — see ErrGlobalMod.
		return nil, fmt.Errorf("%w: %w", ErrGlobalMod, err)
	}
	return g, err
}

// loadGlobal is the body of LoadGlobalWithProfile, split out so every failure
// path gets the ErrGlobalMod tag from one place.
func loadGlobal(profile string) (*Global, error) {
	if GlobalDisabled {
		return nil, ErrNoGlobalMod
	}
	path, err := GlobalModPath()
	if err != nil {
		return nil, err
	}
	f, err := loadFile(path)
	if err != nil {
		return nil, err
	}
	if err := validateGlobalFile(path, f); err != nil {
		return nil, err
	}

	g := &Global{Path: path, Template: f.Sandbox, Policies: f.Policies}
	if g.Template == nil {
		// A file carrying only policy blocks is legitimate: a personal policy
		// library with no defaults of its own.
		g.Template = &Template{}
	}

	if profile != "" {
		overlayPath := path + "." + profile
		overFile, err := maybeParseOverlay(overlayPath)
		if err != nil {
			return nil, err
		}
		if overFile != nil {
			if err := validateGlobalFile(overlayPath, overFile); err != nil {
				return nil, err
			}
			g.ProfileMatched = true
			g.Template.Merge(overFile.Sandbox)
			g.Policies = append(g.Policies, overFile.Policies...)
		}
	}

	// After the overlay merge, so its entries are resolved too — an overlay
	// lives beside the file it extends, so one directory covers both.
	absolutiseHostPaths(g.Template, filepath.Dir(path))
	return g, nil
}

// validateGlobalFile enforces the global scope: the file describes defaults
// for ANY sandbox, so anything that identifies a particular one is rejected by
// name rather than silently ignored. Mirrors how parseNamespaceBlock rejects
// sandbox-level directives and rejectLifecycleAtWorkspace rejects unwired
// hooks — a third scope in the same family.
func validateGlobalFile(path string, f *File) error {
	if len(f.Namespaces) > 0 {
		return fmt.Errorf(
			"%s: 'namespace' blocks are not accepted in the host-wide clawk.mod — "+
				"it declares defaults for every sandbox, not named resources, and clawk "+
				"owns the namespace records itself (a hand-edited copy here would be overwritten)",
			path)
	}
	tmpl := f.Sandbox
	if tmpl == nil {
		return nil
	}
	switch {
	case tmpl.Name != "":
		// The header name is the repo/phase label (it lands in Repo.Name and
		// names worktrees), not the sandbox's name — so a name here would
		// silently relabel every repo on the host.
		return fmt.Errorf(
			"%s: the host-wide clawk.mod's sandbox block must be anonymous — "+
				"write `sandbox ( … )`; a header name labels one repo's phases, which is meaningless for defaults",
			path)
	case len(tmpl.Includes) > 0:
		return fmt.Errorf(
			"%s: 'includes' declares a workspace root and cannot be host-wide — "+
				"it would pull those repos into every sandbox",
			path)
	case len(tmpl.OnDown) > 0:
		return fmt.Errorf("%s: 'on down' is reserved and wired nowhere yet", path)
	case len(tmpl.OnEnter) > 0:
		return fmt.Errorf("%s: 'on enter' is reserved and wired nowhere yet", path)
	}
	return nil
}

// absolutiseHostPaths rewrites every host-side relative path in tmpl to be
// absolute against dir — the global file's own directory.
//
// This is what lets the layer be merged as a plain base template: once
// resolved, a `files ( ./x )` or `agent ( instructions ./house-rules.md )`
// entry no longer needs to remember which file declared it, so the downstream
// compose steps (which resolve relative paths against the repo root, or the
// process CWD) reach the right file with no provenance plumbing.
//
// Guest-side paths, ~ / $HOME prefixes and URLs are left alone; expanding ~
// here would hide the user's spelling from error messages for no gain.
func absolutiseHostPaths(tmpl *Template, dir string) {
	if tmpl == nil {
		return
	}
	for i := range tmpl.Files {
		tmpl.Files[i].HostPath = absHostPath(dir, tmpl.Files[i].HostPath)
	}
	for i := range tmpl.Shares {
		tmpl.Shares[i].HostPath = absHostPath(dir, tmpl.Shares[i].HostPath)
	}
	for i := range tmpl.Serials {
		tmpl.Serials[i].HostPath = absHostPath(dir, tmpl.Serials[i].HostPath)
	}
	for i := range tmpl.Instructions {
		tmpl.Instructions[i].Path = absHostPath(dir, tmpl.Instructions[i].Path)
	}
	for i := range tmpl.Memory {
		tmpl.Memory[i].Path = absHostPath(dir, tmpl.Memory[i].Path)
	}
	// `vm ( kernel … )` takes a path OR an http(s) URL; only the former moves.
	if !isURL(tmpl.Kernel) {
		tmpl.Kernel = absHostPath(dir, tmpl.Kernel)
	}
}

// absHostPath makes p absolute against dir, leaving empty values, absolute
// paths and home-relative spellings (~, $HOME) untouched.
func absHostPath(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if strings.HasPrefix(p, "~") || strings.HasPrefix(p, "$HOME") {
		return p
	}
	return filepath.Join(dir, p)
}

// isURL reports whether s is an http(s) URL (a kernel may be either).
func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// foldGlobalUnderOnlyRepo layers the global defaults under the sole repo's
// clawk.mod, so the repo's scalars win and its list entries follow the global
// ones. Repo.Clawkfile may be nil — a repo with no clawk.mod is exactly the
// case the host-wide layer exists for — and then the layer becomes its whole
// template.
//
// Only correct for a single-repo workspace: folding into N repos would
// duplicate agent instructions and run every global hook once per phase. See
// foldGlobalIntoWorkspace for the multi-repo shape.
func foldGlobalUnderOnlyRepo(ws *Workspace, g *Global) {
	if len(ws.Repos) != 1 {
		return
	}
	base := g.Template.Clone()
	base.Merge(ws.Repos[0].Clawkfile)
	ws.Repos[0].Clawkfile = base
	// A repo whose name came from its own block header keeps it; the global
	// file is required to be anonymous, so there is nothing to inherit.
}

// foldGlobalIntoWorkspace layers the global defaults under a workspace root's
// own sandbox block.
//
// The workspace position is the right one for a multi-repo sandbox: its
// `files`/`shares`/`env` compose once for the VM and its `on up` / `on create`
// run at the guest workspace root, whereas folding the layer into each repo
// would duplicate agent instructions and run every hook once per phase.
//
// The catch is precedence. resolveProvider / resolveImage / resolveKernel and
// the resource resolvers consult ws.File FIRST, by design: a workspace root
// exists to settle disagreements between its repos. A default must not inherit
// that authority, so any scalar that arrived from the global layer is dropped
// again when some repo declares its own — leaving the global value to apply
// only where nothing narrower did.
//
// That decision is made against the FULL repo list, before any later
// FilterRepos (`--only`). So a global scalar yielded to a repo that a subsequent
// --only excludes stays yielded, and the provider default applies instead of
// the host's. Deliberate: re-deriving it per selection would make one repo's
// resolved shape depend on which siblings came along.
func foldGlobalIntoWorkspace(ws *Workspace, g *Global) {
	own := ws.File // what the workspace file (plus its profile overlay) declared
	base := g.Template.Clone()
	base.Merge(own)
	ws.File = base

	if own.Provider == "" && anyRepo(ws, func(t *Template) bool { return t.Provider != "" }) {
		ws.File.Provider = ""
	}
	if own.Image == "" && anyRepo(ws, func(t *Template) bool { return t.Image != "" }) {
		ws.File.Image = ""
	}
	if own.Kernel == "" && anyRepo(ws, func(t *Template) bool { return t.Kernel != "" }) {
		ws.File.Kernel = ""
	}
	if own.CPU == 0 && anyRepo(ws, func(t *Template) bool { return t.CPU != 0 }) {
		ws.File.CPU = 0
	}
	if own.MemoryMiB == 0 && anyRepo(ws, func(t *Template) bool { return t.MemoryMiB != 0 }) {
		ws.File.MemoryMiB = 0
	}
	if own.MemoryMaxMiB == 0 && anyRepo(ws, func(t *Template) bool { return t.MemoryMaxMiB != 0 }) {
		ws.File.MemoryMaxMiB = 0
	}
	if own.DiskMiB == 0 && anyRepo(ws, func(t *Template) bool { return t.DiskMiB != 0 }) {
		ws.File.DiskMiB = 0
	}
	if own.SwapMiB == 0 && anyRepo(ws, func(t *Template) bool { return t.SwapMiB != 0 }) {
		ws.File.SwapMiB = 0
	}
	if own.IdleTimeoutSec == 0 && anyRepo(ws, func(t *Template) bool { return t.IdleTimeoutSec != 0 }) {
		ws.File.IdleTimeoutSec = 0
	}
	// Nested is deliberately left as the union: it is opt-in-only everywhere
	// (there is no `nested false`), so a host that asks for it gets it.
}

// anyRepo reports whether some repo's clawk.mod satisfies pred.
func anyRepo(ws *Workspace, pred func(*Template) bool) bool {
	for _, r := range ws.Repos {
		if r.Clawkfile != nil && pred(r.Clawkfile) {
			return true
		}
	}
	return false
}

// attachGlobal loads the host-wide layer and hands it to fold, recording the
// path on the workspace for the create-time note. A missing file is not an
// error; anything else (unreadable, unparseable, out-of-scope directive) is —
// silently ignoring a broken defaults file would leave the user staring at a
// sandbox that quietly lacks half its configuration.
func attachGlobal(ws *Workspace, profile string, fold func(*Workspace, *Global)) error {
	g, err := LoadGlobalWithProfile(profile)
	if errors.Is(err, ErrNoGlobalMod) {
		return nil
	}
	if err != nil {
		return err
	}
	fold(ws, g)
	ws.GlobalPath = g.Path
	ws.GlobalProfileMatched = g.ProfileMatched
	ws.Policies = append(g.Policies, ws.Policies...)
	return nil
}
