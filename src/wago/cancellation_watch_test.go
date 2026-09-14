package wago

import (
	"context"
	"errors"
	"testing"

	wruntime "github.com/wago-org/wago/src/core/runtime"
	"github.com/wago-org/wago/tests/support/wasmtest"
)

func TestCancellationWatchInertContexts(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{{"nil", nil}, {"background", context.Background()}, {"todo", context.TODO()}, {"without-cancel", context.WithoutCancel(context.Background())}} {
		t.Run(tc.name, func(t *testing.T) {
			var in Instance
			trap := []byte{0, 0, 0, 0}
			allocs := testing.AllocsPerRun(100, func() {
				stop, err := in.startCancellationWatch(tc.ctx, trap)
				if err != nil {
					t.Fatal(err)
				}
				stop()
				stop()
			})
			if allocs != 0 {
				t.Fatalf("inert watcher allocates: %g allocs/op", allocs)
			}
		})
	}
}

func TestNativeEntryRetainsPendingCancellation(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*Instance, uintptr) error
	}{
		{"synchronous", func(in *Instance, entry uintptr) error {
			return in.callNativeSync(entry)
		}},
		{"prepared shared control", func(in *Instance, entry uintptr) error {
			return in.callNativeAsyncWithTrap(entry, true, in.trap)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := newInvocationContextTestRuntime(t, new(invocationContextTestState))
			defer rt.Close()
			module, err := rt.Compile(wasmtest.Module(
				wasmtest.Section(1, wasmtest.Vec(wasmtest.FuncType(nil, nil))),
				wasmtest.Section(2, wasmtest.Vec(importEntry("env", "outer", 0, 0))),
				wasmtest.Section(3, wasmtest.Vec(wasmtest.ULEB(0))),
				wasmtest.Section(7, wasmtest.Vec(wasmtest.ExportEntry("nop", 0, 1))),
				wasmtest.Section(10, wasmtest.Vec(wasmtest.Code([]byte{0x0b}))),
			))
			if err != nil {
				t.Fatal(err)
			}
			in, err := rt.Instantiate(context.Background(), module)
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()

			// A cross-instance call can leave foreign control installed before
			// cancellation arrives. Restoring this entry must keep that signal.
			foreignTrap := make([]byte, wruntime.TrapBufferBytes)
			if err := in.jm.BindTrapCell(foreignTrap); err != nil {
				t.Fatal(err)
			}
			wruntime.RequestInterrupt(in.trap)
			err = test.call(in, in.base+uintptr(in.c.Entry[0]))
			var trap *wruntime.TrapError
			if !errors.As(err, &trap) || trap.Code != wruntime.TrapInterrupted {
				t.Fatalf("native entry discarded pending cancellation: %v", err)
			}
		})
	}
}

func BenchmarkCancellationWatch(b *testing.B) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{{"background", context.Background()}, {"todo", context.TODO()}, {"without-cancel", context.WithoutCancel(context.Background())}} {
		b.Run(tc.name, func(b *testing.B) {
			var in Instance
			trap := make([]byte, 4)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stop, err := in.startCancellationWatch(tc.ctx, trap)
				if err != nil {
					b.Fatal(err)
				}
				stop()
			}
		})
	}
}
