package upstream

import (
	"fmt"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestDesktopHardwareStableAndBounded 同一账号的机型参数必须稳定，且落在平台
// 合法区间（偶数核 [4,32]、内存 8/16/32G、osVersion 取自轮换表）。
func TestDesktopHardwareStableAndBounded(t *testing.T) {
	t.Parallel()

	versions := make(map[string]bool, len(desktopOSVersions))
	for _, ver := range desktopOSVersions {
		versions[ver.build] = true
	}

	for i := range 24 {
		account := &auth.Auth{UID: fmt.Sprintf("uid-%02d", i)}

		osVersion, cpuCores, memorySize := desktopHardware(account)
		osAgain, coresAgain, memAgain := desktopHardware(account)

		if osVersion != osAgain || cpuCores != coresAgain || memorySize != memAgain {
			t.Fatalf("uid=%s 机型参数不稳定：%s/%d/%d vs %s/%d/%d",
				account.UID, osVersion, cpuCores, memorySize, osAgain, coresAgain, memAgain)
		}

		assertDesktopHardware(t, account.UID, osVersion, cpuCores, memorySize, versions)
	}
}

// assertDesktopHardware 校验一组机型参数的取值范围。
func assertDesktopHardware(t *testing.T, uid, osVersion string, cpuCores, memorySize int, versions map[string]bool) {
	t.Helper()

	if !versions[osVersion] {
		t.Errorf("uid=%s osVersion=%q 不在轮换表内", uid, osVersion)
	}

	if cpuCores < 4 || cpuCores > 32 || cpuCores%2 != 0 {
		t.Errorf("uid=%s cpuCores=%d want 偶数且落在 [4,32]", uid, cpuCores)
	}

	if memorySize != 8 && memorySize != 16 && memorySize != 32 {
		t.Errorf("uid=%s memorySize=%d want 8/16/32", uid, memorySize)
	}
}

// TestDesktopHardwareVariesAcrossAccounts 不同账号不应退化成同一组机型参数
// （否则「按账号派生」形同虚设）。
func TestDesktopHardwareVariesAcrossAccounts(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, 16)

	for i := range 16 {
		account := &auth.Auth{UID: fmt.Sprintf("uid-%02d", i)}
		osVersion, cpuCores, memorySize := desktopHardware(account)

		seen[fmt.Sprintf("%s/%d/%d", osVersion, cpuCores, memorySize)] = true
	}

	if len(seen) < 2 {
		t.Fatalf("不同账号的机型参数全部相同：%v", seen)
	}
}
