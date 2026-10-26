// Package cpupool 提供客户端指纹用的 CPU 型号池，按物理核心数或逻辑线程数分档。
//
// 数据来源（2026-10-22 逐条核对）：
//   - Intel ARK 产品规格库：https://www.intel.com/content/www/us/en/ark.html
//   - AMD 处理器规格页：https://www.amd.com/en/products/specifications/processors.html
//   - PassMark CPU Benchmark：https://www.cpubenchmark.net/（交叉核对核心/线程数）
//
// 每条 CPUModel 的 Source 指向该型号的厂商官方规格页，Cores/Threads 与页面
// 「Total Cores / Total Threads」（Intel）或「# of CPU Cores / # of Threads」
// （AMD）一致。型号名采用 Windows 任务管理器/系统信息中的常见展示形式。
package cpupool

import "slices"

// Segment 表示处理器所属的厂商产品线分档。
type Segment uint8

const (
	// Desktop 桌面处理器。
	Desktop Segment = iota
	// Mobile 移动/笔记本处理器。
	Mobile
)

// CPUModel 是一款 CPU 的基本规格与来源。
type CPUModel struct {
	Name    string  // 标准 Windows 展示名称（如 /proc/cpuinfo / CPU-Z 同形）
	Cores   int     // 物理核心数 (P核 + E核)
	Threads int     // 逻辑处理器/线程数
	Segment Segment // 桌面或移动
	Year    int     // 型号发布年，用于约束 OS 版本与新平台组合
	Source  string  // 厂商官方规格页
}

const (
	intelARKBase = "https://www.intel.com/content/www/us/en/products/sku/"
	amdBase      = "https://www.amd.com"
)

// intelARK 由 SKU/产品 slug 拼接 Intel ARK 规格页：
// https://www.intel.com/content/www/us/en/products/sku/<path>/specifications.html
func intelARK(path string) string {
	return intelARKBase + path + "/specifications.html"
}

// amdProduct 由产品线、系列与型号 slug 拼接 AMD 产品规格页：
// https://www.amd.com/en/products/processors/<segment>/ryzen/<series>-series/<slug>.html
func amdProduct(segment, series, slug string) string {
	return amdBase + "/en/products/processors/" + segment + "/ryzen/" + series + "-series/" + slug + ".html"
}

// amdSupport 由系列与型号 slug 拼接 AMD 支持页（老型号的产品页已下线，但其
// 支持页仍保留完整 General Specifications，作为官方规格来源）：
// https://www.amd.com/en/support/downloads/drivers.html/processors/ryzen/ryzen-<series>-series/<slug>.html
func amdSupport(series, slug string) string {
	return amdBase + "/en/support/downloads/drivers.html/processors/ryzen/ryzen-" +
		series + "-series/" + slug + ".html"
}

// Models 是全部已核对的型号；桌面与移动合并，按需按核心/线程分档。
var Models = []CPUModel{
	// ---------------- Intel 桌面 ----------------
	{
		Name: "Intel(R) Core(TM) i3-8100 CPU @ 3.60GHz", Cores: 4, Threads: 4, Segment: Desktop, Year: 2017,
		Source: intelARK("126688/intel-core-i38100-processor-6m-cache-3-60-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i3-9100 CPU @ 3.60GHz", Cores: 4, Threads: 4, Segment: Desktop, Year: 2019,
		Source: intelARK("134870/intel-core-i39100-processor-6m-cache-up-to-4-20-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i3-10100 CPU @ 3.60GHz", Cores: 4, Threads: 8, Segment: Desktop, Year: 2020,
		Source: intelARK("199283/intel-core-i310100-processor-6m-cache-up-to-4-30-ghz"),
	},
	{
		Name: "12th Gen Intel(R) Core(TM) i3-12100", Cores: 4, Threads: 8, Segment: Desktop, Year: 2022,
		Source: intelARK("134584/intel-core-i312100-processor-12m-cache-up-to-4-30-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i5-8400 CPU @ 2.80GHz", Cores: 6, Threads: 6, Segment: Desktop, Year: 2017,
		Source: intelARK("126687/intel-core-i58400-processor-9m-cache-up-to-4-00-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i5-9400F CPU @ 2.90GHz", Cores: 6, Threads: 6, Segment: Desktop, Year: 2019,
		Source: intelARK("190883/intel-core-i59400f-processor-9m-cache-up-to-4-10-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i5-10400 CPU @ 2.90GHz", Cores: 6, Threads: 12, Segment: Desktop, Year: 2020,
		Source: intelARK("199271/intel-core-i510400-processor-12m-cache-up-to-4-30-ghz"),
	},
	{
		Name: "11th Gen Intel(R) Core(TM) i5-11400 @ 2.60GHz", Cores: 6, Threads: 12, Segment: Desktop, Year: 2021,
		Source: intelARK("212270/intel-core-i511400-processor-12m-cache-up-to-4-40-ghz"),
	},
	{
		Name: "12th Gen Intel(R) Core(TM) i5-12400", Cores: 6, Threads: 12, Segment: Desktop, Year: 2022,
		Source: intelARK("134586/intel-core-i512400-processor-18m-cache-up-to-4-40-ghz"),
	},
	{
		Name: "13th Gen Intel(R) Core(TM) i5-13400", Cores: 10, Threads: 16, Segment: Desktop, Year: 2023,
		Source: intelARK("230495/intel-core-i513400-processor-20m-cache-up-to-4-60-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i5-14400", Cores: 10, Threads: 16, Segment: Desktop, Year: 2024,
		Source: intelARK("236788/intel-core-i5-processor-14400-20m-cache-up-to-4-70-ghz"),
	},
	{
		Name: "13th Gen Intel(R) Core(TM) i5-13600K", Cores: 14, Threads: 20, Segment: Desktop, Year: 2022,
		Source: intelARK("230493/intel-core-i513600k-processor-24m-cache-up-to-5-10-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i5-14600K", Cores: 14, Threads: 20, Segment: Desktop, Year: 2023,
		Source: intelARK("236799/intel-core-i5-processor-14600k-24m-cache-up-to-5-30-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i7-9700 CPU @ 3.00GHz", Cores: 8, Threads: 8, Segment: Desktop, Year: 2019,
		Source: intelARK("191792/intel-core-i79700-processor-12m-cache-up-to-4-70-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i7-10700 CPU @ 2.90GHz", Cores: 8, Threads: 16, Segment: Desktop, Year: 2020,
		Source: intelARK("199316/intel-core-i710700-processor-16m-cache-up-to-4-80-ghz"),
	},
	{
		Name: "11th Gen Intel(R) Core(TM) i7-11700K @ 3.60GHz", Cores: 8, Threads: 16, Segment: Desktop, Year: 2021,
		Source: intelARK("212047/intel-core-i711700k-processor-16m-cache-up-to-5-00-ghz"),
	},
	{
		Name: "12th Gen Intel(R) Core(TM) i7-12700", Cores: 12, Threads: 20, Segment: Desktop, Year: 2022,
		Source: intelARK("134591/intel-core-i712700-processor-25m-cache-up-to-4-90-ghz"),
	},
	{
		Name: "13th Gen Intel(R) Core(TM) i7-13700", Cores: 16, Threads: 24, Segment: Desktop, Year: 2023,
		Source: intelARK("230490/intel-core-i713700-processor-30m-cache-up-to-5-20-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i7-14700K", Cores: 20, Threads: 28, Segment: Desktop, Year: 2023,
		Source: intelARK("236783/intel-core-i7-processor-14700k-33m-cache-up-to-5-60-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i9-10900K CPU @ 3.70GHz", Cores: 10, Threads: 20, Segment: Desktop, Year: 2020,
		Source: intelARK("199332/intel-core-i910900k-processor-20m-cache-up-to-5-30-ghz"),
	},
	{
		Name: "13th Gen Intel(R) Core(TM) i9-13900K", Cores: 24, Threads: 32, Segment: Desktop, Year: 2022,
		Source: intelARK("230496/intel-core-i913900k-processor-36m-cache-up-to-5-80-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i9-14900K", Cores: 24, Threads: 32, Segment: Desktop, Year: 2023,
		Source: intelARK("236773/intel-core-i9-processor-14900k-36m-cache-up-to-6-00-ghz"),
	},

	// ---------------- Intel 移动 ----------------
	{
		Name: "Intel(R) N100", Cores: 4, Threads: 4, Segment: Mobile, Year: 2023,
		Source: intelARK("231803/intel-processor-n100-6m-cache-up-to-3-40-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i5-8250U CPU @ 1.60GHz", Cores: 4, Threads: 8, Segment: Mobile, Year: 2017,
		Source: intelARK("124967/intel-core-i58250u-processor-6m-cache-up-to-3-40-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i7-8550U CPU @ 1.80GHz", Cores: 4, Threads: 8, Segment: Mobile, Year: 2017,
		Source: intelARK("122589/intel-core-i78550u-processor-8m-cache-up-to-4-00-ghz"),
	},
	{
		Name: "11th Gen Intel(R) Core(TM) i7-1165G7 @ 2.80GHz", Cores: 4, Threads: 8, Segment: Mobile, Year: 2020,
		Source: intelARK("208662/intel-core-i71165g7-processor-12m-cache-up-to-4-70-ghz"),
	},
	{
		Name: "11th Gen Intel(R) Core(TM) i5-11400H @ 2.70GHz", Cores: 6, Threads: 12, Segment: Mobile, Year: 2021,
		Source: intelARK("213805/intel-core-i511400h-processor-12m-cache-up-to-4-50-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz", Cores: 6, Threads: 12, Segment: Mobile, Year: 2019,
		Source: intelARK("191045/intel-core-i79750h-processor-12m-cache-up-to-4-50-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i7-10875H CPU @ 2.30GHz", Cores: 8, Threads: 16, Segment: Mobile, Year: 2020,
		Source: intelARK("202329/intel-core-i710875h-processor-16m-cache-up-to-5-10-ghz"),
	},
	{
		Name: "11th Gen Intel(R) Core(TM) i7-11800H @ 2.30GHz", Cores: 8, Threads: 16, Segment: Mobile, Year: 2021,
		Source: intelARK("213803/intel-core-i711800h-processor-24m-cache-up-to-4-60-ghz"),
	},
	{
		Name: "12th Gen Intel(R) Core(TM) i5-12500H", Cores: 12, Threads: 16, Segment: Mobile, Year: 2022,
		Source: intelARK("96141/intel-core-i512500h-processor-18m-cache-up-to-4-50-ghz"),
	},
	{
		Name: "12th Gen Intel(R) Core(TM) i7-12700H", Cores: 14, Threads: 20, Segment: Mobile, Year: 2022,
		Source: intelARK("132228/intel-core-i712700h-processor-24m-cache-up-to-4-70-ghz"),
	},
	{
		Name: "12th Gen Intel(R) Core(TM) i7-12800H", Cores: 14, Threads: 20, Segment: Mobile, Year: 2022,
		Source: intelARK("226059/intel-core-i712800h-processor-24m-cache-up-to-4-80-ghz"),
	},
	{
		Name: "12th Gen Intel(R) Core(TM) i9-12900H", Cores: 14, Threads: 20, Segment: Mobile, Year: 2022,
		Source: intelARK("132214/intel-core-i912900h-processor-24m-cache-up-to-5-00-ghz"),
	},
	{
		Name: "12th Gen Intel(R) Core(TM) i7-12800HX", Cores: 16, Threads: 24, Segment: Mobile, Year: 2022,
		Source: intelARK("226058/intel-core-i712800hx-processor-25m-cache-up-to-4-80-ghz"),
	},
	{
		Name: "12th Gen Intel(R) Core(TM) i9-12950HX", Cores: 16, Threads: 24, Segment: Mobile, Year: 2022,
		Source: intelARK("228439/intel-core-i912950hx-processor-30m-cache-up-to-5-00-ghz"),
	},
	{
		Name: "13th Gen Intel(R) Core(TM) i7-13620H", Cores: 10, Threads: 16, Segment: Mobile, Year: 2023,
		Source: intelARK("232130/intel-core-i713620h-processor-24m-cache-up-to-4-90-ghz"),
	},
	{
		Name: "13th Gen Intel(R) Core(TM) i9-13900HX", Cores: 24, Threads: 32, Segment: Mobile, Year: 2023,
		Source: intelARK("232171/intel-core-i913900hx-processor-36m-cache-up-to-5-40-ghz"),
	},
	{
		Name: "Intel(R) Core(TM) i9-14900HX", Cores: 24, Threads: 32, Segment: Mobile, Year: 2024,
		Source: intelARK("235995/intel-core-i9-processor-14900hx-36m-cache-up-to-5-80-ghz"),
	},

	// ---------------- AMD 桌面 ----------------
	{
		Name: "AMD Ryzen 3 3100 4-Core Processor", Cores: 4, Threads: 8, Segment: Desktop, Year: 2020,
		Source: amdSupport("3000", "amd-ryzen-3-3100"),
	},
	{
		Name: "AMD Ryzen 3 3300X 4-Core Processor", Cores: 4, Threads: 8, Segment: Desktop, Year: 2020,
		Source: amdSupport("3000", "amd-ryzen-3-3300x"),
	},
	{
		Name: "AMD Ryzen 5 3600 6-Core Processor", Cores: 6, Threads: 12, Segment: Desktop, Year: 2019,
		Source: amdSupport("3000", "amd-ryzen-5-3600"),
	},
	{
		Name: "AMD Ryzen 5 5600X 6-Core Processor", Cores: 6, Threads: 12, Segment: Desktop, Year: 2020,
		Source: amdProduct("desktops", "5000", "amd-ryzen-5-5600x"),
	},
	{
		Name: "AMD Ryzen 5 7600X 6-Core Processor", Cores: 6, Threads: 12, Segment: Desktop, Year: 2022,
		Source: amdProduct("desktops", "7000", "amd-ryzen-5-7600x"),
	},
	{
		Name: "AMD Ryzen 5 9600X 6-Core Processor", Cores: 6, Threads: 12, Segment: Desktop, Year: 2024,
		Source: amdProduct("desktops", "9000", "amd-ryzen-5-9600x"),
	},
	{
		Name: "AMD Ryzen 7 3700X 8-Core Processor", Cores: 8, Threads: 16, Segment: Desktop, Year: 2019,
		Source: amdSupport("3000", "amd-ryzen-7-3700x"),
	},
	{
		Name: "AMD Ryzen 7 5700X 8-Core Processor", Cores: 8, Threads: 16, Segment: Desktop, Year: 2022,
		Source: amdSupport("5000", "amd-ryzen-7-5700x"),
	},
	{
		Name: "AMD Ryzen 7 5800X 8-Core Processor", Cores: 8, Threads: 16, Segment: Desktop, Year: 2020,
		Source: amdProduct("desktops", "5000", "amd-ryzen-7-5800x"),
	},
	{
		Name: "AMD Ryzen 7 7800X3D 8-Core Processor", Cores: 8, Threads: 16, Segment: Desktop, Year: 2023,
		Source: amdProduct("desktops", "7000", "amd-ryzen-7-7800x3d"),
	},
	{
		Name: "AMD Ryzen 7 9700X 8-Core Processor", Cores: 8, Threads: 16, Segment: Desktop, Year: 2024,
		Source: amdProduct("desktops", "9000", "amd-ryzen-7-9700x"),
	},
	{
		Name: "AMD Ryzen 9 3900X 12-Core Processor", Cores: 12, Threads: 24, Segment: Desktop, Year: 2019,
		Source: amdSupport("3000", "amd-ryzen-9-3900x"),
	},
	{
		Name: "AMD Ryzen 9 5900X 12-Core Processor", Cores: 12, Threads: 24, Segment: Desktop, Year: 2020,
		Source: amdProduct("desktops", "5000", "amd-ryzen-9-5900x"),
	},
	{
		Name: "AMD Ryzen 9 7900X 12-Core Processor", Cores: 12, Threads: 24, Segment: Desktop, Year: 2022,
		Source: amdProduct("desktops", "7000", "amd-ryzen-9-7900x"),
	},
	{
		Name: "AMD Ryzen 9 9900X 12-Core Processor", Cores: 12, Threads: 24, Segment: Desktop, Year: 2024,
		Source: amdProduct("desktops", "9000", "amd-ryzen-9-9900x"),
	},
	{
		Name: "AMD Ryzen 9 3950X 16-Core Processor", Cores: 16, Threads: 32, Segment: Desktop, Year: 2019,
		Source: amdSupport("3000", "amd-ryzen-9-3950x"),
	},
	{
		Name: "AMD Ryzen 9 5950X 16-Core Processor", Cores: 16, Threads: 32, Segment: Desktop, Year: 2020,
		Source: amdProduct("desktops", "5000", "amd-ryzen-9-5950x"),
	},
	{
		Name: "AMD Ryzen 9 7950X 16-Core Processor", Cores: 16, Threads: 32, Segment: Desktop, Year: 2022,
		Source: amdProduct("desktops", "7000", "amd-ryzen-9-7950x"),
	},
	{
		Name: "AMD Ryzen 9 9950X 16-Core Processor", Cores: 16, Threads: 32, Segment: Desktop, Year: 2024,
		Source: amdProduct("desktops", "9000", "amd-ryzen-9-9950x"),
	},

	// ---------------- AMD 移动 ----------------
	{
		Name: "AMD Ryzen 3 5300U with Radeon Graphics", Cores: 4, Threads: 8, Segment: Mobile, Year: 2021,
		Source: amdSupport("5000", "amd-ryzen-3-5300u"),
	},
	{
		Name: "AMD Ryzen 5 3500U with Radeon Vega Mobile Gfx", Cores: 4, Threads: 8, Segment: Mobile, Year: 2019,
		Source: amdSupport("3000", "amd-ryzen-5-3500u"),
	},
	{
		Name: "AMD Ryzen 5 4600H with Radeon Graphics", Cores: 6, Threads: 12, Segment: Mobile, Year: 2020,
		Source: amdSupport("4000", "amd-ryzen-5-4600h"),
	},
	{
		Name: "AMD Ryzen 5 5600H with Radeon Graphics", Cores: 6, Threads: 12, Segment: Mobile, Year: 2021,
		Source: amdSupport("5000", "amd-ryzen-5-5600h"),
	},
	{
		Name: "AMD Ryzen 7 4800H with Radeon Graphics", Cores: 8, Threads: 16, Segment: Mobile, Year: 2020,
		Source: amdSupport("4000", "amd-ryzen-7-4800h"),
	},
	{
		Name: "AMD Ryzen 7 5800H with Radeon Graphics", Cores: 8, Threads: 16, Segment: Mobile, Year: 2021,
		Source: amdSupport("5000", "amd-ryzen-7-5800h"),
	},
	{
		Name: "AMD Ryzen 7 6800H with Radeon Graphics", Cores: 8, Threads: 16, Segment: Mobile, Year: 2022,
		Source: amdSupport("6000", "amd-ryzen-7-6800h"),
	},
	{
		Name: "AMD Ryzen 7 7840HS w/ Radeon 780M Graphics", Cores: 8, Threads: 16, Segment: Mobile, Year: 2023,
		Source: amdProduct("laptop", "7000", "amd-ryzen-7-7840hs"),
	},
	{
		Name: "AMD Ryzen 7 8845HS w/ Radeon 780M Graphics", Cores: 8, Threads: 16, Segment: Mobile, Year: 2024,
		Source: amdProduct("laptop", "8000", "amd-ryzen-7-8845hs"),
	},
	{
		Name: "AMD Ryzen 9 5900HX with Radeon Graphics", Cores: 8, Threads: 16, Segment: Mobile, Year: 2021,
		Source: amdSupport("5000", "amd-ryzen-9-5900hx"),
	},
	{
		Name: "AMD Ryzen 9 6900HX with Radeon Graphics", Cores: 8, Threads: 16, Segment: Mobile, Year: 2022,
		Source: amdSupport("6000", "amd-ryzen-9-6900hx"),
	},
	{
		Name: "AMD Ryzen 9 7945HX with Radeon Graphics", Cores: 16, Threads: 32, Segment: Mobile, Year: 2023,
		Source: amdProduct("laptop", "7000", "amd-ryzen-9-7945hx"),
	},
}

// band 是某一核心数/线程数档位下的型号集合。
type band struct {
	value  int
	models []CPUModel
}

// groupBy 按 key 对 models 分档，返回按档位升序排列的结果。
func groupBy(models []CPUModel, key func(CPUModel) int) []band {
	buckets := make(map[int][]CPUModel, len(models))
	for _, model := range models {
		value := key(model)
		buckets[value] = append(buckets[value], model)
	}

	values := make([]int, 0, len(buckets))
	for value := range buckets {
		values = append(values, value)
	}

	slices.Sort(values)

	bands := make([]band, 0, len(values))
	for _, value := range values {
		bands = append(bands, band{value: value, models: buckets[value]})
	}

	return bands
}

var (
	coresBands  = groupBy(Models, func(model CPUModel) int { return model.Cores })
	threadBands = groupBy(Models, func(model CPUModel) int { return model.Threads })
)

// pickBand 返回第一个档位不小于 value 的型号池；超出最大档时取最大档。
func pickBand(bands []band, value int) []CPUModel {
	for _, entry := range bands {
		if value <= entry.value {
			return entry.models
		}
	}

	return bands[len(bands)-1].models
}

// SnapThreads 把任意逻辑线程数向上归到最近的档位（4/6/8/12/16/20/24/28/32），
// 保证上报的 cpuCores 与所选 CPU 型号的线程数一致；超出最大档时取最大档。
func SnapThreads(threads int) int {
	for _, entry := range threadBands {
		if threads <= entry.value {
			return entry.value
		}
	}

	return threadBands[len(threadBands)-1].value
}

// ModelsForThreads 返回逻辑线程数 threads 对应的型号池；超出最大档时取最大档。
func ModelsForThreads(threads int) []CPUModel {
	return pickBand(threadBands, threads)
}
