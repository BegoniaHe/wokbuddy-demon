// Package ospool 提供桌面客户端指纹用的 Windows 版本池。
//
// 数据来源（2026-10-26 核对）：
//   - Windows 11: https://learn.microsoft.com/en-us/windows/release-health/windows11-release-information
//   - Windows 10: https://learn.microsoft.com/en-us/windows/release-health/release-information
//
// Windows 11 收当前版本表中仍有主流存量的 GA 版本（23H2/24H2 消费版已结束
// 更新，但企业版仍在服务期且装机量可观）；Windows 10 只收 22H2（19045，ESU
// 已于 2026-10-13 结束，但存量设备升级缓慢），不收 LTSC 与更老的消费版。
// Build 取主 build 号（不含 .revision），与桌面事件 osVersion 的上报形态一致。
// Weight 是近似真实保有量的挑选权重，非微软页面字段；页面更新后按最新版本表
// 同步本列表。
//
// 26H1（28000）仅预装于 ARM 新机，而桌面指纹固定 os=win32/arch=x64，故不收，
// 避免上报「Arm-only 系统 + x64」的矛盾组合。
package ospool

import (
	"math/rand/v2"
)

// Family 版本所属的 Windows 大版本。
const (
	FamilyWindows10 = "Windows 10"
	FamilyWindows11 = "Windows 11"
)

// SourceURL 各 family 的官方版本信息页。
const (
	Windows10SourceURL = "https://learn.microsoft.com/en-us/windows/release-health/release-information"
	Windows11SourceURL = "https://learn.microsoft.com/en-us/windows/release-health/windows11-release-information"
)

// Release 是一个 Windows 版本及其主 build 号。
type Release struct {
	Family  string // Windows 10 / Windows 11
	Version string // 版本名，如 25H2
	Build   string // 主 build 号，如 10.0.26200
	Weight  int    // 近似真实保有量权重
	Source  string // 官方版本信息页
}

// Releases 是当前仍在真实设备中占主流的 Windows 版本；按权重挑选。
var Releases = []Release{
	{Family: FamilyWindows11, Version: "26H2", Build: "10.0.26300", Weight: 12, Source: Windows11SourceURL},
	{Family: FamilyWindows11, Version: "25H2", Build: "10.0.26200", Weight: 34, Source: Windows11SourceURL},
	{Family: FamilyWindows11, Version: "24H2", Build: "10.0.26100", Weight: 30, Source: Windows11SourceURL},
	{Family: FamilyWindows11, Version: "23H2", Build: "10.0.22631", Weight: 16, Source: Windows11SourceURL},
	{Family: FamilyWindows10, Version: "22H2", Build: "10.0.19045", Weight: 40, Source: Windows10SourceURL},
}

// Pick 按 Weight 加权随机选一个版本；rng 由调用方按账号稳定派生。
func Pick(rng *rand.Rand) Release {
	total := 0
	for _, release := range Releases {
		total += release.Weight
	}

	pick := rng.IntN(total)

	for _, release := range Releases {
		if pick < release.Weight {
			return release
		}

		pick -= release.Weight
	}

	return Releases[len(Releases)-1]
}
