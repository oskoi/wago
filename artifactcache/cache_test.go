package artifactcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/wago-org/wago"
	corewasm "github.com/wago-org/wago/src/core/compiler/wasm"
	"github.com/wago-org/wago/tests/support/wasmtest"
)

func TestLoadOrCompileCachesAndRepairsArtifact(t *testing.T) {
	source := constantModule()
	config := wago.NewRuntimeConfig().WithBoundsChecks(wago.BoundsChecksExplicit)
	cache := Cache{Dir: t.TempDir(), Identity: []byte("runtime-a")}
	rt := wago.NewRuntime(wago.WithRuntimeConfig(config))
	defer rt.Close()

	first, err := cache.LoadOrCompile(source, rt)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if first.Compiled().Exports["answer"] != 0 {
		t.Fatalf("unexpected exports: %#v", first.Compiled().Exports)
	}
	path, ok := cache.path(source, config)
	if !ok {
		t.Fatal("cache key unavailable")
	}
	artifact, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("artifact was not cached: %v", err)
	}
	if !wago.IsCompiled(artifact) {
		t.Fatal("cached file is not a .wago artifact")
	}

	if err := os.WriteFile(path, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	recompiled, err := cache.LoadOrCompile(source, rt)
	if err != nil {
		t.Fatal(err)
	}
	defer recompiled.Close()
	repaired, err := os.ReadFile(path)
	if err != nil || !wago.IsCompiled(repaired) {
		t.Fatalf("corrupt cache was not repaired: compiled=%v err=%v", wago.IsCompiled(repaired), err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".wago-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary artifacts remain: %v (err %v)", matches, err)
	}
}

func TestLoadArtifactRejectsSymlinkAndOversizeEntry(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.wago")
	if err := os.WriteFile(target, []byte("not an artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.wago")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if compiled, hit := loadArtifact(link); hit || compiled != nil {
		t.Fatalf("symlink cache entry loaded: %v, %v", compiled, hit)
	}

	oversize := filepath.Join(dir, "oversize.wago")
	limits := wago.DefaultArtifactLimits()
	file, err := os.Create(oversize)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(oversize, limits.MaxCodeBytes+limits.MaxMetadataBytes+65); err != nil {
		t.Fatal(err)
	}
	if compiled, hit := loadArtifact(oversize); hit || compiled != nil {
		t.Fatalf("oversize cache entry loaded: %v, %v", compiled, hit)
	}
}

func TestLoadOpenedArtifactRejectsReplacementAndGrowth(t *testing.T) {
	source := constantModule()
	compiled, err := wago.Compile(wago.NewRuntimeConfig().WithBoundsChecks(wago.BoundsChecksExplicit), source)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := compiled.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.Close(); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name        string
		skipWindows bool
		mutate      func(string, *os.File) error
	}{
		{name: "replacement", skipWindows: true, mutate: func(path string, _ *os.File) error {
			replacement := path + ".replacement"
			if err := os.WriteFile(replacement, artifact, 0o644); err != nil {
				return err
			}
			return os.Rename(replacement, path)
		}},
		{name: "growth", mutate: func(_ string, file *os.File) error {
			return os.Truncate(file.Name(), int64(len(artifact)+1))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.skipWindows && runtime.GOOS == "windows" {
				t.Skip("Windows does not replace an open cache entry by rename")
			}
			path := filepath.Join(t.TempDir(), "entry.wago")
			if err := os.WriteFile(path, artifact, 0o644); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			opened, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err := test.mutate(path, file); err != nil {
				t.Fatal(err)
			}
			if loaded, hit := loadOpenedArtifact(path, file, opened); hit || loaded != nil {
				t.Fatalf("mutated cache entry loaded: %v, %v", loaded, hit)
			}
		})
	}
}

func TestLoadOrCompileReportsPublicationFailure(t *testing.T) {
	source := constantModule()
	config := wago.NewRuntimeConfig().WithBoundsChecks(wago.BoundsChecksExplicit)
	cache := Cache{Dir: t.TempDir(), Identity: []byte("runtime-a")}
	var reported error
	cache.ReportError = func(err error) { reported = err }
	rt := wago.NewRuntime(wago.WithRuntimeConfig(config))
	defer rt.Close()

	injected := errors.New("injected cache publication failure")
	oldPublish := publishArtifact
	publishArtifact = func(string, *wago.Compiled) error { return injected }
	t.Cleanup(func() { publishArtifact = oldPublish })

	module, err := cache.LoadOrCompile(source, rt)
	if err != nil || module == nil {
		t.Fatalf("LoadOrCompile = %v, %v", module, err)
	}
	defer module.Close()
	if !errors.Is(reported, injected) {
		t.Fatalf("reported error = %v", reported)
	}
}

type cachePlugin func(*wago.Registrar) error

func (f cachePlugin) Register(reg *wago.Registrar) error { return f(reg) }

func loadCachePlugin(t testing.TB, rt *wago.Runtime, id string, authorities []wago.AuthorityRequest, register cachePlugin) {
	t.Helper()
	definition := wago.PluginDefinition{
		ID: id, Version: "1.0.0",
		Provenance:  wago.PluginProvenance{Repository: "https://example.com/" + id, License: "MIT"},
		Authorities: authorities,
	}
	digest, err := wago.DefinitionDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	selection := wago.PluginSelection{ID: id, DefinitionDigest: digest, Direct: true, Dependencies: map[string]string{}}
	for _, authority := range authorities {
		if authority.Mode == wago.AuthorityRequired {
			selection.Grants = append(selection.Grants, wago.AuthorityGrant{Name: authority.Name, Scope: authority.Scope})
		}
	}
	if err := rt.LoadPlugins(context.Background(), wago.PluginSet{
		Providers:  []wago.PluginProvider{{Definition: definition, New: func() wago.Plugin { return register }}},
		Selections: []wago.PluginSelection{selection},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCacheHitPropagatesAfterCompileErrorExactlyOnce(t *testing.T) {
	source := constantModule()
	config := wago.NewRuntimeConfig().WithBoundsChecks(wago.BoundsChecksExplicit)
	cache := Cache{Dir: t.TempDir(), Identity: []byte("runtime-a")}
	seedRuntime := wago.NewRuntime(wago.WithRuntimeConfig(config))
	module, err := cache.LoadOrCompile(source, seedRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
	if err := seedRuntime.Close(); err != nil {
		t.Fatal(err)
	}

	rejected := errors.New("cached artifact rejected")
	calls := 0
	rt := wago.NewRuntime(wago.WithRuntimeConfig(config))
	defer rt.Close()
	loadCachePlugin(t, rt, "example.com/cache/reject", []wago.AuthorityRequest{{
		Name: wago.AuthorityModuleCompileObserve, Mode: wago.AuthorityRequired, Reason: "reject adopted artifacts",
	}}, func(reg *wago.Registrar) error {
		observer, err := reg.ModuleCompileObserver()
		if err != nil {
			return err
		}
		return observer.Observe(func(wago.ModuleCompiledEvent) {
			calls++
			if calls == 1 {
				panic(rejected)
			}
		})
	})
	if _, err := cache.LoadOrCompile(source, rt); !errors.Is(err, rejected) {
		t.Fatalf("cache binding error = %v, want %v", err, rejected)
	}
	if calls != 1 {
		t.Fatalf("AfterCompile calls = %d, want 1", calls)
	}
}

func TestCacheHitPropagatesRuntimeBindingError(t *testing.T) {
	source := constantModule()
	config := wago.NewRuntimeConfig().WithBoundsChecks(wago.BoundsChecksExplicit)
	cache := Cache{Dir: t.TempDir(), Identity: []byte("runtime-a")}
	seedRuntime := wago.NewRuntime(wago.WithRuntimeConfig(config))
	module, err := cache.LoadOrCompile(source, seedRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
	if err := seedRuntime.Close(); err != nil {
		t.Fatal(err)
	}

	closedRuntime := wago.NewRuntime(wago.WithRuntimeConfig(config))
	if err := closedRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.LoadOrCompile(source, closedRuntime); err == nil || !strings.Contains(err.Error(), "closed runtime") {
		t.Fatalf("cache binding error = %v", err)
	}
}

func TestLoadOrCompileBypassesArtifactsForCompileOnlyTelemetry(t *testing.T) {
	source := constantModule()
	base := wago.NewRuntimeConfig().WithBoundsChecks(wago.BoundsChecksExplicit)
	cache := Cache{Dir: t.TempDir(), Identity: []byte("runtime-a")}
	seedRuntime := wago.NewRuntime(wago.WithRuntimeConfig(base))
	seed, err := cache.LoadOrCompile(source, seedRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := seedRuntime.Close(); err != nil {
		t.Fatal(err)
	}

	telemetry := base.WithGCCodeTelemetry(true)
	rt := wago.NewRuntime(wago.WithRuntimeConfig(telemetry))
	module, err := cache.LoadOrCompile(source, rt)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := module.Compiled().GCNativeCodeTelemetry(); !ok {
		t.Fatal("fresh telemetry compile did not retain requested attribution")
	}
	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCacheKeyIncludesRuntimeAndCompilerConfiguration(t *testing.T) {
	source := constantModule()
	dir := t.TempDir()
	base := wago.NewRuntimeConfig().WithBoundsChecks(wago.BoundsChecksExplicit)
	featureOff := base.WithFeature(wago.CoreFeatureSIMD, false)
	knob := base.OptimizationInfos()[0]
	optimizationOff := base.WithOptimization(knob.Name, !knob.On)
	workers := base.WithFunctionWorkers(2)
	nativeStack := base.WithNativeStackBytes(8 << 20)
	bounds := base.WithBoundsChecks(wago.BoundsChecksSignalsBased)
	deferredOff := base.WithDeferBoundsChecks(false)
	memoryLimit := base.WithMemoryLimitPages(1)
	localLimit := base.WithMaxFunctionLocals(base.MaxFunctionLocals() - 1)
	memoryCountLimit := base.WithMaxMemoriesPerModule(base.MaxMemoriesPerModule() - 1)

	basePath, ok := (Cache{Dir: dir, Identity: []byte("runtime-a")}).path(source, base)
	if !ok {
		t.Fatal("base cache key unavailable")
	}
	featurePath, _ := (Cache{Dir: dir, Identity: []byte("runtime-a")}).path(source, featureOff)
	optimizationPath, _ := (Cache{Dir: dir, Identity: []byte("runtime-a")}).path(source, optimizationOff)
	workersPath, _ := (Cache{Dir: dir, Identity: []byte("runtime-a")}).path(source, workers)
	nativeStackPath, _ := (Cache{Dir: dir, Identity: []byte("runtime-a")}).path(source, nativeStack)
	boundsPath, _ := (Cache{Dir: dir, Identity: []byte("runtime-a")}).path(source, bounds)
	deferredPath, _ := (Cache{Dir: dir, Identity: []byte("runtime-a")}).path(source, deferredOff)
	memoryPath, _ := (Cache{Dir: dir, Identity: []byte("runtime-a")}).path(source, memoryLimit)
	localPath, _ := (Cache{Dir: dir, Identity: []byte("runtime-a")}).path(source, localLimit)
	memoryCountPath, _ := (Cache{Dir: dir, Identity: []byte("runtime-a")}).path(source, memoryCountLimit)
	runtimePath, _ := (Cache{Dir: dir, Identity: []byte("runtime-b")}).path(source, base)
	sourcePath, _ := (Cache{Dir: dir, Identity: []byte("runtime-a")}).path(append(source, 0), base)
	if basePath == featurePath {
		t.Fatal("feature configuration did not change artifact key")
	}
	if basePath == runtimePath {
		t.Fatal("runtime identity did not change artifact key")
	}
	if basePath == optimizationPath {
		t.Fatal("optimization selection did not change artifact key")
	}
	if basePath != workersPath {
		t.Fatal("function-worker scheduling policy changed artifact key")
	}
	if basePath != nativeStackPath {
		t.Fatal("runtime-only native stack capacity changed artifact key")
	}
	if basePath == boundsPath {
		t.Fatal("bounds-check mode did not change artifact key")
	}
	if basePath == deferredPath {
		t.Fatal("deferred-bounds policy did not change artifact key")
	}
	if basePath != memoryPath {
		t.Fatal("runtime-only memory page quota changed artifact key")
	}
	if basePath == localPath {
		t.Fatal("function local limit did not change artifact key")
	}
	if basePath == memoryCountPath {
		t.Fatal("module memory count limit did not change artifact key")
	}
	if basePath == sourcePath {
		t.Fatal("source bytes did not change artifact key")
	}
}

func cacheMemoryQuotaModule() []byte {
	return wasmtest.Module(
		wasmtest.Section(1, wasmtest.Vec(wasmtest.FuncType([]corewasm.ValType{corewasm.I32}, []corewasm.ValType{corewasm.I32}))),
		wasmtest.Section(3, wasmtest.Vec(wasmtest.ULEB(0))),
		wasmtest.Section(5, wasmtest.Vec([]byte{0x01, 0x01, 0x03})),
		wasmtest.Section(7, wasmtest.Vec(wasmtest.ExportEntry("grow", byte(corewasm.ExternFunc), 0))),
		wasmtest.Section(10, wasmtest.Vec(wasmtest.Code([]byte{0x20, 0x00, 0x40, 0x00, 0x0b}))),
	)
}

func TestCachedArtifactCannotBypassStricterMemoryPageQuota(t *testing.T) {
	source := cacheMemoryQuotaModule()
	cache := Cache{Dir: t.TempDir(), Identity: []byte("runtime-a")}
	unlimited := wago.NewRuntimeConfig().WithBoundsChecks(wago.BoundsChecksExplicit)
	seed := wago.NewRuntime(wago.WithRuntimeConfig(unlimited))
	mod, err := cache.LoadOrCompile(source, seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := mod.Close(); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	strict := unlimited.WithMemoryLimitPages(1)
	rt := wago.NewRuntime(wago.WithRuntimeConfig(strict))
	defer rt.Close()
	mod, err = cache.LoadOrCompile(source, rt)
	if err != nil {
		t.Fatal(err)
	}
	defer mod.Close()
	in, err := rt.Instantiate(context.Background(), mod)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	values, err := in.Invoke("grow", wago.I32(1))
	if err != nil || len(values) != 1 || uint32(values[0]) != ^uint32(0) {
		t.Fatalf("cached artifact growth past strict quota = %v, %v", values, err)
	}
}

func requireCacheResourceLimit(t *testing.T, cache Cache, source []byte, rt *wago.Runtime, resource string, requested, limit uint64) {
	t.Helper()
	module, err := cache.LoadOrCompile(source, rt)
	if module != nil || !errors.Is(err, wago.ErrResourceLimit) {
		if module != nil {
			_ = module.Close()
		}
		t.Fatalf("cache resource limit = %v, %v; want %s resource limit", module, err, resource)
	}
	var limitErr *wago.ResourceLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != resource || limitErr.Scope != "compile" || limitErr.Requested != requested || limitErr.Limit != limit {
		t.Fatalf("cache resource limit = %#v; want %s requested=%d limit=%d", err, resource, requested, limit)
	}
}

func TestCachedArtifactCannotBypassStricterNativeCodeQuota(t *testing.T) {
	source := constantModule()
	cache := Cache{Dir: t.TempDir(), Identity: []byte("runtime-a")}
	base := wago.NewRuntimeConfig().WithBoundsChecks(wago.BoundsChecksExplicit)
	seed := wago.NewRuntime(wago.WithRuntimeConfig(base))
	seedModule, err := cache.LoadOrCompile(source, seed)
	if err != nil {
		t.Fatal(err)
	}
	codeBytes := seedModule.Compiled().CodeSize()
	if err := seedModule.Close(); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	if codeBytes < 2 {
		t.Fatalf("quota fixture native code bytes = %d", codeBytes)
	}

	strict := base.WithMaxNativeCodeBytes(uint64(codeBytes - 1))
	seedPath, ok := cache.path(source, base)
	if !ok {
		t.Fatal("seed cache key unavailable")
	}
	strictPath, ok := cache.path(source, strict)
	if !ok || strictPath != seedPath {
		t.Fatalf("strict cache key = %q, want warm key %q", strictPath, seedPath)
	}
	for _, test := range []struct {
		name  string
		cache Cache
	}{
		{name: "cold", cache: Cache{Dir: t.TempDir(), Identity: []byte("runtime-a")}},
		{name: "warm", cache: cache},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := wago.NewRuntime(wago.WithRuntimeConfig(strict))
			defer rt.Close()
			requireCacheResourceLimit(t, test.cache, source, rt, "native code bytes", uint64(codeBytes), uint64(codeBytes-1))
		})
	}

	exact := base.WithMaxNativeCodeBytes(uint64(codeBytes))
	rt := wago.NewRuntime(wago.WithRuntimeConfig(exact))
	defer rt.Close()
	module, err := cache.LoadOrCompile(source, rt)
	if err != nil || module == nil {
		t.Fatalf("exact native-code quota = %v, %v", module, err)
	}
	defer module.Close()
}

func TestCachedArtifactCannotBypassStricterModuleByteQuota(t *testing.T) {
	source := constantModule()
	cache := Cache{Dir: t.TempDir(), Identity: []byte("runtime-a")}
	base := wago.NewRuntimeConfig().WithBoundsChecks(wago.BoundsChecksExplicit)
	seed := wago.NewRuntime(wago.WithRuntimeConfig(base))
	module, err := cache.LoadOrCompile(source, seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := module.Close(); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	bytes := uint64(len(source))
	strict := base.WithMaxModuleBytes(bytes - 1)
	seedPath, ok := cache.path(source, base)
	if !ok {
		t.Fatal("seed cache key unavailable")
	}
	strictPath, ok := cache.path(source, strict)
	if !ok || strictPath != seedPath {
		t.Fatalf("strict cache key = %q, want warm key %q", strictPath, seedPath)
	}
	for _, test := range []struct {
		name  string
		cache Cache
	}{
		{name: "cold", cache: Cache{Dir: t.TempDir(), Identity: []byte("runtime-a")}},
		{name: "warm", cache: cache},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := wago.NewRuntime(wago.WithRuntimeConfig(strict))
			defer rt.Close()
			requireCacheResourceLimit(t, test.cache, source, rt, "module bytes", bytes, bytes-1)
		})
	}

	exact := base.WithMaxModuleBytes(bytes)
	rt := wago.NewRuntime(wago.WithRuntimeConfig(exact))
	defer rt.Close()
	module, err = cache.LoadOrCompile(source, rt)
	if err != nil || module == nil {
		t.Fatalf("exact module-byte quota = %v, %v", module, err)
	}
	defer module.Close()
}

func TestBuildIdentityIgnoresEmbeddingApplicationMetadata(t *testing.T) {
	base := &debug.BuildInfo{
		GoVersion: "go1.25.0",
		Path:      "example.com/embed",
		Main:      debug.Module{Path: "example.com/embed", Version: "v1.0.0", Sum: "h1:embed"},
		Deps: []*debug.Module{
			{Path: wagoModulePath, Version: "v1.2.3", Sum: "h1:wago"},
			{Path: "example.com/extension", Version: "v1.0.0", Sum: "h1:extension"},
		},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"},
			{Key: "vcs.modified", Value: "false"},
			{Key: "-tags", Value: "release"},
		},
	}
	want := buildIdentity(base)

	changed := *base
	changed.Path = "example.com/another/embed"
	changed.Main = debug.Module{Path: "example.com/another/embed", Version: "v9.9.9", Sum: "h1:another-embed"}
	changed.Deps = []*debug.Module{
		{Path: wagoModulePath, Version: "v1.2.3", Sum: "h1:other-wago"},
		{Path: "example.com/local-extension", Version: "(devel)"},
	}
	changed.Settings = []debug.BuildSetting{
		{Key: "vcs.revision", Value: "fedcba9876543210"},
		{Key: "vcs.modified", Value: "true"},
		{Key: "GOAMD64", Value: "v4"},
		{Key: "-trimpath", Value: "true"},
	}
	if got := buildIdentity(&changed); got != want {
		t.Fatal("embedding application metadata changed compiler identity")
	}
}

func TestBuildIdentitySeparatesCompilerVersionsAndForks(t *testing.T) {
	identity := func(goVersion string, module debug.Module) [sha256.Size]byte {
		return buildIdentity(&debug.BuildInfo{GoVersion: goVersion, Deps: []*debug.Module{&module}})
	}
	canonical := debug.Module{Path: wagoModulePath, Version: "v1.2.3", Sum: "h1:wago"}
	want := identity("go1.25.0", canonical)

	if got := identity("go1.25.0", debug.Module{Path: wagoModulePath, Version: "v1.2.4", Sum: "h1:wago-next"}); got == want {
		t.Fatal("Wago module version did not change compiler identity")
	}
	if got := identity("go1.26.0", canonical); got == want {
		t.Fatal("Go compiler version did not change compiler identity")
	}

	fork := debug.Module{
		Path: wagoModulePath, Version: "v0.9.0", Sum: "h1:required-wago",
		Replace: &debug.Module{Path: "example.com/fork/wago", Version: "v1.2.3", Sum: "h1:fork"},
	}
	forkIdentity := identity("go1.25.0", fork)
	sameFork := fork
	sameFork.Version = "v9.9.9"
	sameFork.Sum = "h1:other-required-wago"
	if got := identity("go1.25.0", sameFork); got != forkIdentity {
		t.Fatal("versioned replacement used the original required version")
	}
	fork.Replace = &debug.Module{Path: "example.com/other-fork/wago", Version: "v1.2.3", Sum: "h1:fork"}
	if got := identity("go1.25.0", fork); got == forkIdentity {
		t.Fatal("fork module path did not change compiler identity")
	}
	fork.Replace = &debug.Module{Path: "example.com/fork/wago", Version: "v1.2.4", Sum: "h1:fork-next"}
	if got := identity("go1.25.0", fork); got == forkIdentity {
		t.Fatal("fork module version did not change compiler identity")
	}
}

func TestBuildIdentityNormalizesDevelopmentEngine(t *testing.T) {
	const goVersion = "go1.25.0"
	identity := func(module *debug.Module) [sha256.Size]byte {
		info := &debug.BuildInfo{GoVersion: goVersion}
		if module != nil {
			info.Deps = []*debug.Module{module}
		}
		return buildIdentity(info)
	}
	dev := identity(nil)
	cases := []struct {
		name   string
		module *debug.Module
	}{
		{name: "missing engine", module: &debug.Module{Path: "example.com/other", Version: "v1.0.0"}},
		{name: "empty engine version", module: &debug.Module{Path: wagoModulePath}},
		{name: "development engine version", module: &debug.Module{Path: wagoModulePath, Version: "(devel)"}},
		{name: "dirty compiler version", module: &debug.Module{Path: wagoModulePath, Version: "v0.0.0-20260912185342-781ea39a3915+dirty"}},
		{name: "first local replacement", module: &debug.Module{
			Path: wagoModulePath, Version: "v1.2.3", Replace: &debug.Module{Path: "../first-wago"},
		}},
		{name: "second local replacement", module: &debug.Module{
			Path: wagoModulePath, Version: "v9.9.9", Replace: &debug.Module{Path: "/work/second-wago"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := identity(tc.module); got != dev {
				t.Fatal("development engine did not use the shared development identity")
			}
		})
	}
	dirtyMain := &debug.BuildInfo{
		GoVersion: goVersion,
		Main:      debug.Module{Path: wagoModulePath, Version: "v0.0.0-20260912185342-781ea39a3915+dirty"},
	}
	if got := buildIdentity(dirtyMain); got != dev {
		t.Fatal("dirty compiler main module did not use the shared development identity")
	}
	if got, want := buildIdentity(nil), buildIdentity(&debug.BuildInfo{GoVersion: runtime.Version()}); got != want {
		t.Fatal("missing Go build metadata did not fall back to runtime version")
	}
}

func BenchmarkCachePath(b *testing.B) {
	cache := Cache{Dir: b.TempDir(), Identity: []byte("benchmark-runtime")}
	config := wago.NewRuntimeConfig()
	source := constantModule()
	b.Run("compact", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, ok := cache.path(source, config); !ok {
				b.Fatal("cache path unavailable")
			}
		}
	})
	b.Run("legacy-json", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, ok := legacyCachePath(cache, source, config); !ok {
				b.Fatal("legacy cache path unavailable")
			}
		}
	})
}

func legacyCachePath(cache Cache, source []byte, config *wago.RuntimeConfig) (string, bool) {
	identity := cache.runtimeIdentity()
	type knob struct {
		Name string `json:"name"`
		On   bool   `json:"on"`
	}
	type signature struct {
		Source             [sha256.Size]byte `json:"source"`
		Runtime            [sha256.Size]byte `json:"runtime"`
		GOOS               string            `json:"goos"`
		GOARCH             string            `json:"goarch"`
		Features           uint64            `json:"features"`
		BoundsChecks       string            `json:"boundsChecks"`
		DeferredBounds     bool              `json:"deferredBoundsChecks"`
		MaximumMemoryPages uint32            `json:"maximumMemoryPages"`
		OptimizationKnobs  []knob            `json:"optimizationKnobs"`
	}
	infos := config.OptimizationInfos()
	knobs := make([]knob, len(infos))
	for i := range infos {
		knobs[i] = knob{Name: infos[i].Name, On: infos[i].On}
	}
	encoded, err := json.Marshal(signature{
		Source:             sha256.Sum256(source),
		Runtime:            identity,
		GOOS:               runtime.GOOS,
		GOARCH:             runtime.GOARCH,
		Features:           uint64(config.CoreFeatures()),
		BoundsChecks:       config.BoundsChecks().String(),
		DeferredBounds:     config.DeferBoundsChecks(),
		MaximumMemoryPages: config.MemoryLimitPages(),
		OptimizationKnobs:  knobs,
	})
	if err != nil {
		return "", false
	}
	key := sha256.Sum256(encoded)
	text := hex.EncodeToString(key[:])
	return filepath.Join(cache.Dir, text[:2], text[2:]+".wago"), true
}

func constantModule() []byte {
	// (module (func (export "answer") (result i32) i32.const 7))
	return []byte{'\x00', 'a', 's', 'm', 1, 0, 0, 0,
		1, 5, 1, 0x60, 0, 1, 0x7f,
		3, 2, 1, 0,
		7, 10, 1, 6, 'a', 'n', 's', 'w', 'e', 'r', 0, 0,
		10, 6, 1, 4, 0, 0x41, 7, 0x0b}
}
