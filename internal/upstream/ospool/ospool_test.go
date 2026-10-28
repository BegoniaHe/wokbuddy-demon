package ospool

import (
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var buildRe = regexp.MustCompile(`^10\.0\.[0-9]{5}$`)

//
//nolint:cyclop // 逐字段校验版本池元数据，线性展开更直观。
func TestReleasesHaveVersionBuildAndSource(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, len(Releases))

	for _, release := range Releases {
		key := release.Family + "/" + release.Version
		if release.Family == "" || release.Version == "" || seen[key] {
			t.Fatalf("family/版本为空或重复: %q", key)
		}

		seen[key] = true

		if !buildRe.MatchString(release.Build) {
			t.Fatalf("%s/%s: build=%q 不是 10.0.<5 位数字> 形态",
				release.Family, release.Version, release.Build)
		}

		if release.Weight <= 0 {
			t.Fatalf("%s/%s: weight=%d 必须为正", release.Family, release.Version, release.Weight)
		}

		if release.MinCPUYear < 2017 || release.MinCPUYear > 2026 {
			t.Fatalf("%s/%s: minCPUYear=%d 超出合理区间",
				release.Family, release.Version, release.MinCPUYear)
		}

		if release.Version == "26H2" && release.MinCPUYear < 2022 {
			t.Fatalf("26H2 要求 2022 年及以后平台，实际门槛 %d", release.MinCPUYear)
		}

		if want := sourceFor(release.Family); release.Source != want {
			t.Fatalf("%s/%s: source=%q want %q",
				release.Family, release.Version, release.Source, want)
		}
	}
}

// sourceFor 返回 family 对应的微软来源页；未知 family 返回空串。
func sourceFor(family string) string {
	switch family {
	case FamilyWindows10:
		return Windows10SourceURL
	case FamilyWindows11:
		return Windows11SourceURL
	default:
		return ""
	}
}

// TestReleaseFamilyMatchesBuild Win10 的 build 号低于 Win11 起始 build（22000），
// 避免把 19045 标成 Win11 或把 22631 标成 Win10。
func TestReleaseFamilyMatchesBuild(t *testing.T) {
	t.Parallel()

	for _, release := range Releases {
		number, err := strconv.Atoi(strings.TrimPrefix(release.Build, "10.0."))
		if err != nil {
			t.Fatalf("%s/%s: build=%q 无法解析: %v",
				release.Family, release.Version, release.Build, err)
		}

		switch release.Family {
		case FamilyWindows10:
			if number >= 22000 {
				t.Fatalf("%s/%s: build=%d 不应属于 Windows 10", release.Family, release.Version, number)
			}
		case FamilyWindows11:
			if number < 22000 {
				t.Fatalf("%s/%s: build=%d 不应属于 Windows 11", release.Family, release.Version, number)
			}
		default:
			t.Fatalf("未知 family: %q", release.Family)
		}
	}
}

func TestPickReturnsReleasedValue(t *testing.T) {
	t.Parallel()

	valid := make(map[string]bool, len(Releases))
	for _, release := range Releases {
		valid[release.Build] = true
	}

	rng := rand.New(rand.NewPCG(1, 2))
	seen := make(map[string]bool, len(Releases))

	for range 200 {
		picked := Pick(rng)
		if !valid[picked.Build] {
			t.Fatalf("Pick 返回不在池内的 build: %q", picked.Build)
		}

		seen[picked.Build] = true
	}

	if len(seen) < 2 {
		t.Fatalf("200 次挑选只命中 %d 个版本，权重分布异常", len(seen))
	}
}

// TestReleasesCoverDesktopTarget 回归 #318：桌面指纹固定 os=Win32/arch=x64，
// 因此版本池不能收入 ARM 独占的 26H1（28000），同时必须覆盖 Windows 10 22H2
// 存量设备。build 不得重复，否则 Pick 的权重分布会偏向重复项。
func TestReleasesCoverDesktopTarget(t *testing.T) {
	t.Parallel()

	byBuild := make(map[string]Release, len(Releases))
	for _, release := range Releases {
		if other, ok := byBuild[release.Build]; ok {
			t.Fatalf("重复 build %q: %s/%s 与 %s/%s",
				release.Build,
				other.Family, other.Version,
				release.Family, release.Version)
		}

		byBuild[release.Build] = release
	}

	if release, ok := byBuild["10.0.28000"]; ok {
		t.Fatalf("不应包含 ARM 独占的 %s %s（%s）", release.Family, release.Version, release.Build)
	}

	win10, ok := byBuild["10.0.19045"]
	if !ok {
		t.Fatal("缺少 Windows 10 22H2（10.0.19045）")
	}

	if win10.Family != FamilyWindows10 || win10.Version != "22H2" {
		t.Fatalf("10.0.19045 应为 Windows 10 22H2，实际 %s %s", win10.Family, win10.Version)
	}
}
