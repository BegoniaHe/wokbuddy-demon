package upstream

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream/cpupool"
)

// TestDesktopHardwareStableAndBounded 同一账号的机型参数必须稳定，且落在平台
// 合法区间（偶数线程 [4,32]、内存 8/16/32/64G、osVersion 与 CPU 年代不矛盾）。
func TestDesktopHardwareStableAndBounded(t *testing.T) {
	t.Parallel()

	versions := make(map[string]int, len(desktopOSVersions))
	for _, ver := range desktopOSVersions {
		versions[ver.build] = ver.minCPUYear
	}

	for i := range 64 {
		account := &auth.Auth{UID: fmt.Sprintf("uid-%02d", i)}

		osVersion, cpu, memorySize := desktopHardware(account)
		osAgain, cpuAgain, memAgain := desktopHardware(account)

		if osVersion != osAgain || cpu != cpuAgain || memorySize != memAgain {
			t.Fatalf("uid=%s 机型参数不稳定：%s/%+v/%d vs %s/%+v/%d",
				account.UID, osVersion, cpu, memorySize, osAgain, cpuAgain, memAgain)
		}

		assertDesktopHardware(t, account.UID, osVersion, cpu, memorySize, versions)
	}
}

// assertDesktopHardware 校验一组机型参数的取值范围与年代/内存相关性。
//
//nolint:cyclop // 逐项校验年代/线程/内存组合，线性展开更直观。
func assertDesktopHardware(
	t *testing.T, uid, osVersion string, cpu cpupool.CPUModel, memorySize int, versions map[string]int,
) {
	t.Helper()

	minYear, ok := versions[osVersion]
	if !ok {
		t.Errorf("uid=%s osVersion=%q 不在轮换表内", uid, osVersion)
	} else if minYear > cpu.Year {
		t.Errorf("uid=%s osVersion=%q 要求 CPU>=%d 年，但型号 %q 是 %d 年",
			uid, osVersion, minYear, cpu.Name, cpu.Year)
	}

	if cpu.Threads < 4 || cpu.Threads > 32 || cpu.Threads%2 != 0 {
		t.Errorf("uid=%s cpu.Threads=%d want 偶数且落在 [4,32]", uid, cpu.Threads)
	}

	if cpu.Year <= 0 {
		t.Errorf("uid=%s cpu.Year=%d want 型号发布年", uid, cpu.Year)
	}

	switch memorySize {
	case 8, 16, 32, 64:
	default:
		t.Errorf("uid=%s memorySize=%d want 8/16/32/64", uid, memorySize)
	}

	if cpu.Segment == cpupool.Mobile && memorySize > 32 {
		t.Errorf("uid=%s 移动段 %q 不得配 %dG", uid, cpu.Name, memorySize)
	}

	if cpu.Threads <= 8 && memorySize > 16 {
		t.Errorf("uid=%s %d 线程配了 %dG（低线程不得配大内存）", uid, cpu.Threads, memorySize)
	}

	if memorySize == 64 && (cpu.Threads < 24 || cpu.Segment == cpupool.Mobile) {
		t.Errorf("uid=%s 64G 只允许 24 线程以上桌面平台，实际 %q/%d 线程",
			uid, cpu.Name, cpu.Threads)
	}

	if memorySize == 8 && cpu.Threads > 6 {
		t.Errorf("uid=%s %d 线程配了 8G（高线程不得配 8G）", uid, cpu.Threads)
	}
}

// TestDesktopHardwareVariesAcrossAccounts 不同账号不应退化成同一组机型参数
// （否则「按账号派生」形同虚设）。
func TestDesktopHardwareVariesAcrossAccounts(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, 16)

	for i := range 16 {
		account := &auth.Auth{UID: fmt.Sprintf("uid-%02d", i)}
		osVersion, cpu, memorySize := desktopHardware(account)

		seen[fmt.Sprintf("%s/%s/%d", osVersion, cpu.Name, memorySize)] = true
	}

	if len(seen) < 2 {
		t.Fatalf("不同账号的机型参数全部相同：%v", seen)
	}
}

// TestDesktopOSVersionCPUYearGate 新版本（26H1/26H2）只能配 2022 年及以后平台；
// 老平台只落在门槛允许的版本里。
func TestDesktopOSVersionCPUYearGate(t *testing.T) {
	t.Parallel()

	minYears := make(map[string]int, len(desktopOSVersions))
	for _, ver := range desktopOSVersions {
		minYears[ver.build] = ver.minCPUYear
	}

	for _, year := range []int{2017, 2018, 2021, 2022, 2024} {
		for seed := range uint64(64) {
			rng := rand.New(rand.NewPCG(seed, seed+1)) //nolint:gosec // 测试固定种子
			build := desktopOSVersionFor(rng, year)

			if minYears[build] > year {
				t.Fatalf("cpuYear=%d 抽到了要求 >=%d 的 %s", year, minYears[build], build)
			}

			if year < 2022 && (build == "10.0.26300" || build == "10.0.28000") {
				t.Fatalf("cpuYear=%d 不应抽到新平台版本 %s", year, build)
			}
		}
	}
}

// TestDesktopMemoryBandsFollowCPU 内存档必须由所选 CPU 型号的线程数与 Segment 决定。
//
//nolint:cyclop // 全量型号 × 多种子的组合校验，线性展开更直观。
func TestDesktopMemoryBandsFollowCPU(t *testing.T) {
	t.Parallel()

	saw64 := false

	for _, cpu := range cpupool.Models {
		for seed := range uint64(16) {
			rng := rand.New(rand.NewPCG(seed, seed+7)) //nolint:gosec // 测试固定种子
			memorySize := desktopMemoryFor(rng, cpu)

			if cpu.Segment == cpupool.Mobile && memorySize > 32 {
				t.Fatalf("移动 %q 配了 %dG", cpu.Name, memorySize)
			}

			if cpu.Threads <= 8 && memorySize > 16 {
				t.Fatalf("%d 线程 %q 配了 %dG", cpu.Threads, cpu.Name, memorySize)
			}

			if memorySize == 8 && cpu.Threads > 6 {
				t.Fatalf("%d 线程 %q 配了 8G", cpu.Threads, cpu.Name)
			}

			if memorySize == 64 {
				saw64 = true

				if cpu.Threads < 24 || cpu.Segment == cpupool.Mobile {
					t.Fatalf("64G 仅限高端桌面，实际 %q/%d 线程", cpu.Name, cpu.Threads)
				}
			}
		}
	}

	if !saw64 {
		t.Fatal("高端桌面型号应能抽到 64G")
	}
}
