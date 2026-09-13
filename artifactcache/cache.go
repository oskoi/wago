// Package artifactcache provides automatic reuse of serialized Wago modules.
package artifactcache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/wago-org/wago"
	"github.com/wago-org/wago/internal/atomicfile"
)

// Cache is a best-effort store for regenerable .wago artifacts.
//
// When Identity is empty, cache keys include the Wago compiler module and Go
// compiler version. A versioned replacement uses the replacement's module path
// and version. Local, unversioned, dirty, and missing Wago module metadata use
// the shared development identity. Reset or remove Dir after changing a
// development engine; those builds cannot distinguish revisions.
type Cache struct {
	// Dir is trusted executable-artifact storage. It must not be writable by
	// untrusted parties. An empty Dir disables persistent caching.
	Dir string
	// Identity overrides the default compiler identity for custom embedders.
	Identity []byte
	// ReportError observes best-effort publication failures. When nil, failures
	// are reported on stderr so persistent cache misses are diagnosable.
	ReportError func(error)
}

const (
	cacheKeyFormat    = 5
	wagoModulePath    = "github.com/wago-org/wago"
	developmentEngine = "dev"
)

var defaultIdentity = sync.OnceValue(func() [sha256.Size]byte {
	info, _ := debug.ReadBuildInfo()
	return buildIdentity(info)
})

// LoadOrCompile loads a matching artifact or compiles source and saves the
// result. Cache read/write failures never prevent execution; compilation and
// artifact validation errors retain their normal behavior.
//
// Source ownership follows native compilation. Callers must not mutate or reuse
// source's backing array while the returned module is live.
func (cache Cache) LoadOrCompile(source []byte, rt *wago.Runtime) (*wago.Module, error) {
	if rt == nil {
		return nil, fmt.Errorf("wago: artifact cache requires a runtime")
	}
	prepared, err := rt.PrepareCompile(source)
	if err != nil {
		return nil, err
	}
	defer prepared.Close()

	var path string
	var cacheable bool
	if prepared.Cacheable() && cache.Dir != "" {
		config := rt.Config()
		// Telemetry is compile-only; signals-based code is nonserializable.
		if !config.GCCodeTelemetry() && config.BoundsChecks() != wago.BoundsChecksSignalsBased {
			path, cacheable = cache.path(prepared.Source(), config)
		}
	}
	if cacheable {
		if compiled, hit := loadArtifact(path); hit {
			return prepared.Adopt(compiled)
		}
	}

	module, err := prepared.Compile()
	if err != nil {
		return nil, err
	}
	if !cacheable {
		return module, nil
	}
	if err := publishArtifact(path, module.Compiled()); err != nil {
		if cache.ReportError != nil {
			cache.ReportError(err)
		} else {
			fmt.Fprintf(os.Stderr, "wago: artifact cache publication failed: %v\n", err)
		}
	}
	return module, nil
}

func (cache Cache) path(source []byte, config *wago.RuntimeConfig) (string, bool) {
	if cache.Dir == "" || config == nil {
		return "", false
	}
	identity := cache.runtimeIdentity()
	var storage [256]byte
	encoded := storage[:0]
	encoded = append(encoded, "wago-artifact-cache"...)
	encoded = binary.LittleEndian.AppendUint32(encoded, cacheKeyFormat)
	encoded = binary.LittleEndian.AppendUint64(encoded, uint64(len(runtime.GOOS)))
	encoded = append(encoded, runtime.GOOS...)
	encoded = binary.LittleEndian.AppendUint64(encoded, uint64(len(runtime.GOARCH)))
	encoded = append(encoded, runtime.GOARCH...)
	encoded = binary.LittleEndian.AppendUint64(encoded, uint64(config.CoreFeatures()))
	encoded = binary.LittleEndian.AppendUint32(encoded, uint32(config.BoundsChecks()))
	encoded = append(encoded, 0)
	if config.DeferBoundsChecks() {
		encoded[len(encoded)-1] = 1
	}
	encoded = binary.LittleEndian.AppendUint32(encoded, config.MaxFunctionLocals())
	encoded = binary.LittleEndian.AppendUint32(encoded, config.MaxMemoriesPerModule())
	knobs := config.OptimizationInfos()
	encoded = binary.LittleEndian.AppendUint32(encoded, uint32(len(knobs)))
	for base := 0; base < len(knobs); base += 8 {
		var selected byte
		for bit := 0; bit < 8 && base+bit < len(knobs); bit++ {
			if knobs[base+bit].On {
				selected |= 1 << bit
			}
		}
		encoded = append(encoded, selected)
	}

	h := sha256.New()
	h.Write(identity[:])
	h.Write(encoded)
	h.Write(source)
	var key [sha256.Size]byte
	h.Sum(key[:0])
	hexKey := hex.EncodeToString(key[:])
	return filepath.Join(cache.Dir, hexKey[:2], hexKey[2:]+".wago"), true
}

func (cache Cache) runtimeIdentity() [sha256.Size]byte {
	if len(cache.Identity) != 0 {
		return sha256.Sum256(cache.Identity)
	}
	return defaultIdentity()
}

func buildIdentity(info *debug.BuildInfo) [sha256.Size]byte {
	goVersion := runtime.Version()
	enginePath := developmentEngine
	engineVersion := ""
	if info != nil {
		if info.GoVersion != "" {
			goVersion = info.GoVersion
		}
		if module := engineModule(info); module != nil {
			for module.Replace != nil {
				module = module.Replace
			}
			if module.Path != "" && module.Path[0] != '.' && !filepath.IsAbs(module.Path) && module.Version != "" && module.Version != "(devel)" && !strings.HasSuffix(module.Version, "+dirty") {
				enginePath = module.Path
				engineVersion = module.Version
			}
		}
	}

	h := sha256.New()
	h.Write([]byte("wago-compiler-identity\x00"))
	h.Write([]byte(goVersion))
	h.Write([]byte{0})
	h.Write([]byte(enginePath))
	h.Write([]byte{0})
	h.Write([]byte(engineVersion))
	var identity [sha256.Size]byte
	h.Sum(identity[:0])
	return identity
}

func engineModule(info *debug.BuildInfo) *debug.Module {
	if info.Main.Path == wagoModulePath {
		return &info.Main
	}
	for _, module := range info.Deps {
		if module != nil && module.Path == wagoModulePath {
			return module
		}
	}
	return nil
}

var publishArtifact = writeAtomic

func loadArtifact(path string) (*wago.Compiled, bool) {
	linked, err := os.Lstat(path)
	if err != nil || linked.Mode()&os.ModeSymlink != 0 || !linked.Mode().IsRegular() {
		return nil, false
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(linked, opened) {
		return nil, false
	}
	return loadOpenedArtifact(path, file, opened)
}

func loadOpenedArtifact(path string, file *os.File, opened os.FileInfo) (*wago.Compiled, bool) {
	limits := wago.DefaultArtifactLimits()
	maximum := limits.MaxCodeBytes + limits.MaxMetadataBytes + 64
	if opened.Size() < 0 || opened.Size() > maximum {
		return nil, false
	}
	compiled := &wago.Compiled{}
	read, err := compiled.ReadFromWithLimits(file, limits)
	if err != nil {
		_ = compiled.Close()
		return nil, false
	}
	var trailing [1]byte
	trailingBytes, trailingErr := file.Read(trailing[:])
	finalOpened, statErr := file.Stat()
	finalLinked, linkErr := os.Lstat(path)
	if read != opened.Size() || trailingBytes != 0 || trailingErr != io.EOF ||
		statErr != nil || finalOpened.Size() != read || !finalOpened.Mode().IsRegular() ||
		linkErr != nil || finalLinked.Mode()&os.ModeSymlink != 0 || !finalLinked.Mode().IsRegular() ||
		!os.SameFile(finalLinked, finalOpened) {
		_ = compiled.Close()
		return nil, false
	}
	return compiled, true
}

func writeAtomic(path string, compiled *wago.Compiled) error {
	return atomicfile.ReplaceFile(path, atomicfile.Options{Mode: 0o644}, func(writer io.Writer) error {
		_, err := compiled.WriteTo(writer)
		return err
	})
}
