package cpupool

import (
	"strings"
	"testing"
)

func TestModelsHaveSpecsAndSource(t *testing.T) {
	t.Parallel()

	seen := make(map[string]string, len(Models))

	for _, model := range Models {
		if model.Name == "" {
			t.Fatal("型号名称为空")
		}

		if prev, ok := seen[model.Source]; ok {
			t.Fatalf("%s 与 %s 来源重复: %s", model.Name, prev, model.Source)
		}

		seen[model.Source] = model.Name

		if model.Cores <= 0 || model.Threads < model.Cores {
			t.Fatalf("%s: cores=%d threads=%d 不合法", model.Name, model.Cores, model.Threads)
		}

		if !strings.HasPrefix(model.Source, "https://") {
			t.Fatalf("%s: source=%q 不是 https 来源", model.Name, model.Source)
		}
	}
}

func TestBandQueriesMatchModelSpecs(t *testing.T) {
	t.Parallel()

	for _, model := range Models {
		for _, got := range ModelsForCores(model.Cores) {
			if got.Cores != model.Cores {
				t.Fatalf("核心档 %d 混入 %s (%d 核)", model.Cores, got.Name, got.Cores)
			}
		}

		for _, got := range ModelsForThreads(model.Threads) {
			if got.Threads != model.Threads {
				t.Fatalf("线程档 %d 混入 %s (%d 线程)", model.Threads, got.Name, got.Threads)
			}
		}
	}
}

func TestSnapThreadsRoundsUp(t *testing.T) {
	t.Parallel()

	for threads := 1; threads <= 32; threads++ {
		if got := SnapThreads(threads); got < threads {
			t.Fatalf("SnapThreads(%d)=%d，未向上归档", threads, got)
		}
	}

	if got := SnapThreads(64); got != 32 {
		t.Fatalf("SnapThreads(64)=%d，超出最大档应取 32", got)
	}
}

// ModelsForCores 测试便利入口：按物理核心数取型号池（生产内部直接走 pickBand）。
func ModelsForCores(cores int) []CPUModel {
	return pickBand(coresBands, cores)
}
