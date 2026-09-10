package controller

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
	"github.com/xuri/excelize/v2"
)

const (
	csvImportChannelName = "CSV/Excel 文件导入"
	maxCSVFileSize       = 10 << 20 // 10MB
)

// csvPriceRow 一行 CSV 计费数据的解析结果
type csvPriceRow struct {
	ModelID  string  // 模型ID
	Desc     string  // 说明（"类型 - 条件"）
	Price    float64 // 标价（元）
	Unit     string  // 单位（百万 Token/秒/次/张/分/页/...）
	Discount float64 // 折扣系数，默认 1（不打折）；合法范围 (0, 1]
}

// priceTier 一个计费变量的单档价格
type priceTier struct {
	UpperBound int64   // 档位上限（tokens），0 表示无阶梯（默认档）
	Price      float64 // 元/百万 tokens
}

// csvDescTypeToVar 说明类型（" - "前缀）→ 计费变量
var csvDescTypeToVar = map[string]string{
	"输入":       "p",
	"文本输入":     "p",
	"文本输出":     "c",
	"缓存":       "cr",
	"缓存读取":     "cr",
	"缓存写入 (5m)": "cc",
	"缓存写入":     "cc",
	"缓存写入 (1h)": "cc1h",
	"图像输入":     "img",
	"图像输出":     "img_o",
	"音频输入":     "ai",
	"音频输出":     "ao",
	"图像缓存读取":   "cr",
	"视频缓存读取":   "cr",
	"音频缓存读取":   "cr",
	"视频输入":     "p",
	"视频输出":     "c",
	"文档输入":     "p",
}

// csvDescTypeSkip 无法映射、直接跳过的说明类型
var csvDescTypeSkip = map[string]bool{
	"缓存存储 (1h)": true, // new-api 不支持缓存存储按时计费
}

// fixedUnitPriority 非 token 类单位优先级（同模型多单位时取优先级最高者）
var fixedUnitPriority = []string{"秒", "次", "张", "分", "页", "百万像素", "字符"}

// exprVarOrder 表达式中变量的固定输出顺序，保证生成结果稳定
var exprVarOrder = []string{"p", "c", "cr", "cc", "cc1h", "img", "img_o", "ai", "ao"}

// exprVarDisplayLabel 计费变量 → 前端 i18n 标签 key（用于差异对比中的可读价格行）
var exprVarDisplayLabel = map[string]string{
	"p":     "Input price",
	"c":     "Output price",
	"cr":    "Cache read price",
	"cc":    "Cache write price",
	"cc1h":  "Cache Write (1h)",
	"img":   "Image input price",
	"img_o": "Image output price",
	"ai":    "Audio input price",
	"ao":    "Audio output price",
}

// displayPriceLine 一条可读价格行：label 为前端 i18n key，value 为价格文本（如 "4.2/8.4 元/M"）
type displayPriceLine struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// tierBoundRegex 匹配长度阶梯上限，如 "输入长度(0, 272K]"；上限为 UNLIMIT 时归入最高档
var tierBoundRegex = regexp.MustCompile(`输入长度\([^,]+,\s*(?:([0-9.]+)\s*([KkMm])?|(UNLIMIT(?:ED)?))\s*\]`)

// parseDesc 将"说明"列拆分为类型与条件，如 "输入 - 输入长度(0, 512K]" → ("输入", "输入长度(0, 512K]")。
// 优先按 " - "（带空格）拆分；拆出的类型无法识别时，回退为已知类型最长前缀匹配，
// 以兼容 "输入-峰时 09:00-12:00（北京时间）" 这类无空格分隔的格式。
func parseDesc(desc string) (typ, cond string) {
	desc = strings.TrimSpace(desc)
	legacyTyp := desc
	legacyCond := ""
	if parts := strings.SplitN(desc, " - ", 2); len(parts) == 2 {
		legacyTyp = strings.TrimSpace(parts[0])
		legacyCond = strings.TrimSpace(parts[1])
	}
	if _, ok := csvDescTypeToVar[legacyTyp]; ok {
		return legacyTyp, legacyCond
	}
	if csvDescTypeSkip[legacyTyp] {
		return legacyTyp, legacyCond
	}
	// 最长前缀匹配：要求前缀后紧跟分隔符（-、–、— 或空白），避免 "输入输出比" 误匹配 "输入"
	best := ""
	for key := range csvDescTypeToVar {
		if len(key) > len(best) && strings.HasPrefix(desc, key) {
			best = key
		}
	}
	for key := range csvDescTypeSkip {
		if len(key) > len(best) && strings.HasPrefix(desc, key) {
			best = key
		}
	}
	if best == "" {
		return legacyTyp, legacyCond
	}
	rest := desc[len(best):]
	if rest != "" && !strings.HasPrefix(rest, "-") && !strings.HasPrefix(rest, "–") &&
		!strings.HasPrefix(rest, "—") && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
		return legacyTyp, legacyCond
	}
	cond = strings.TrimSpace(strings.TrimLeft(rest, "-–—"))
	return best, cond
}

// parseTierUpperBound 从条件中提取阶梯上限，"输入长度(0, 512K]" → 512000；
// 上限为 UNLIMIT 时返回 math.MaxInt64（归入最高档）；无阶梯返回 0
func parseTierUpperBound(cond string) int64 {
	m := tierBoundRegex.FindStringSubmatch(cond)
	if m == nil {
		return 0
	}
	if m[3] != "" {
		return math.MaxInt64
	}
	num, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(m[2]) {
	case "K":
		num *= 1000
	case "M":
		num *= 1000000
	}
	return int64(num)
}

// ---------------------------------------------------------------------------
// 峰谷时段条件解析
// ---------------------------------------------------------------------------

// minuteRange 一天内的分钟数区间 [Start, End)，End <= Start 表示跨午夜（如 18:00-次日08:00）
type minuteRange struct {
	Start int
	End   int
}

// dateTimeRange 绝对日期时间范围（忽略年份，仅月日+当天分钟数），
// 用于 "指定时段：2026-09-01 08:00 至 2026-12-01 08:00" 这类限时促销条件
type dateTimeRange struct {
	StartMD   int // 起始月日（月*100+日，如 901）
	StartMins int // 起始当天分钟数
	EndMD     int
	EndMins   int
}

// timePeriodCond 一个计费时段条件：标签 + 时区 + 若干分钟数区间
type timePeriodCond struct {
	Label    string // 时段标签（如 "峰时"/"谷时"），用作 tier label
	Timezone string // IANA 时区（如 "Asia/Shanghai"）
	Ranges   []minuteRange
	// WeekRanges 周级分钟数区间（周一 00:00 为 0，周日=0/周六=6 按 Go Weekday 计），
	// 用于 "每周：周六 00:00-周一 00:00" 这类按星期循环的条件；
	// 与 Ranges（每日时段）之间为或关系，End <= Start 表示跨周边界
	WeekRanges []minuteRange
	// DateRange 绝对日期范围；存在时与其他时段部分为与关系（在日期范围内且命中时段）
	DateRange *dateTimeRange
}

var timeRangeRegex = regexp.MustCompile(`(\d{1,2}):(\d{2})\s*-\s*(次日)?(\d{1,2}):(\d{2})`)
var tzTextRegex = regexp.MustCompile(`[（(]([^（）()]*)[）)]`)

// weekRangeRegex 匹配周级时段，如 "周六 00:00-周一 00:00"
var weekRangeRegex = regexp.MustCompile(`(周[一二三四五六日天])\s*(\d{1,2}):(\d{2})\s*-\s*(周[一二三四五六日天])\s*(\d{1,2}):(\d{2})`)
var weeklyMarkerRegex = regexp.MustCompile(`每周[：:]`)
var dailyMarkerRegex = regexp.MustCompile(`每日[：:]`)

// dateRangeRegex 匹配绝对日期范围，如 "2026-09-01 08:00 至 2026-12-01 08:00"
var dateRangeRegex = regexp.MustCompile(`(\d{4})-(\d{1,2})-(\d{1,2})\s+(\d{1,2}):(\d{2})\s*至\s*(\d{4})-(\d{1,2})-(\d{1,2})\s+(\d{1,2}):(\d{2})`)
var dateMarkerRegex = regexp.MustCompile(`指定时段[：:]`)

// csvWeekdayNames 星期中文名 → Go time.Weekday（周日=0）
var csvWeekdayNames = map[string]int{
	"周日": 0, "周天": 0,
	"周一": 1, "周二": 2, "周三": 3, "周四": 4, "周五": 5, "周六": 6,
}

// csvTimezoneAliases 说明文字中的时区别名 → IANA 时区
var csvTimezoneAliases = map[string]string{
	"北京时间":  "Asia/Shanghai",
	"北京":    "Asia/Shanghai",
	"中国标准时间": "Asia/Shanghai",
	"Asia/Shanghai": "Asia/Shanghai",
	"UTC+8":   "Asia/Shanghai",
	"utc+8":   "Asia/Shanghai",
}

// parseTimePeriodCondition 从条件文字解析时段条件，
// 如 "峰时 09:00-12:00、14:00-18:00（北京时间）"。
// 非时段条件（无时间范围）返回 (nil, nil)；结构类似但内容非法时返回错误。
func parseTimePeriodCondition(cond string) (*timePeriodCond, error) {
	if cond == "" || cond == "默认" {
		return nil, nil
	}
	rest := cond
	tz := "Asia/Shanghai" // 未标注时区时默认北京时间
	if m := tzTextRegex.FindStringSubmatch(rest); m != nil {
		text := strings.TrimSpace(m[1])
		mapped, ok := csvTimezoneAliases[text]
		if !ok {
			return nil, fmt.Errorf("无法识别的时区: %q", text)
		}
		tz = mapped
		rest = tzTextRegex.ReplaceAllString(rest, "")
	}

	// 绝对日期范围段："指定时段：2026-09-01 08:00 至 2026-12-01 08:00"（限时促销等）
	var dateRange *dateTimeRange
	if m := dateRangeRegex.FindStringSubmatch(rest); m != nil {
		startMonth, _ := strconv.Atoi(m[2])
		startDay, _ := strconv.Atoi(m[3])
		sh, _ := strconv.Atoi(m[4])
		sm, _ := strconv.Atoi(m[5])
		endMonth, _ := strconv.Atoi(m[7])
		endDay, _ := strconv.Atoi(m[8])
		eh, _ := strconv.Atoi(m[9])
		em, _ := strconv.Atoi(m[10])
		if startMonth < 1 || startMonth > 12 || endMonth < 1 || endMonth > 12 ||
			startDay < 1 || startDay > 31 || endDay < 1 || endDay > 31 ||
			sh > 23 || eh > 23 || sm > 59 || em > 59 {
			return nil, fmt.Errorf("指定时段超出合法日期时间: %q", cond)
		}
		dr := &dateTimeRange{
			StartMD: startMonth*100 + startDay, StartMins: sh*60 + sm,
			EndMD: endMonth*100 + endDay, EndMins: eh*60 + em,
		}
		if dr.StartMD > dr.EndMD || dr.StartMD == dr.EndMD && dr.StartMins >= dr.EndMins {
			return nil, fmt.Errorf("指定时段起止顺序无效: %q", cond)
		}
		dateRange = dr
		rest = dateRangeRegex.ReplaceAllString(rest, "")
		rest = dateMarkerRegex.ReplaceAllString(rest, "")
	}

	// 周级条件段："每周：周六 00:00-周一 00:00"，可与 "每日：…" 组合（两者为或关系）
	var weekRanges []minuteRange
	if loc := weeklyMarkerRegex.FindStringIndex(rest); loc != nil {
		seg := rest[loc[1]:]
		end := len(seg)
		if i := strings.IndexAny(seg, "；;"); i >= 0 {
			end = i
		}
		weekSeg := seg[:end]
		rest = rest[:loc[0]] + seg[end:]
		weekMatches := weekRangeRegex.FindAllStringSubmatch(weekSeg, -1)
		if len(weekMatches) == 0 {
			return nil, fmt.Errorf("每周时段条件无法识别: %q", cond)
		}
		for _, m := range weekMatches {
			startDay, okStart := csvWeekdayNames[m[1]]
			endDay, okEnd := csvWeekdayNames[m[4]]
			if !okStart || !okEnd {
				return nil, fmt.Errorf("无法识别的星期: %q", cond)
			}
			sh, _ := strconv.Atoi(m[2])
			sm, _ := strconv.Atoi(m[3])
			eh, _ := strconv.Atoi(m[5])
			em, _ := strconv.Atoi(m[6])
			if sh > 23 || eh > 23 || sm > 59 || em > 59 {
				return nil, fmt.Errorf("每周时段超出合法时间: %q", cond)
			}
			start := startDay*24*60 + sh*60 + sm
			end := endDay*24*60 + eh*60 + em
			if start == end {
				return nil, fmt.Errorf("每周时段起止时间相同: %q", cond)
			}
			weekRanges = append(weekRanges, minuteRange{Start: start, End: end})
		}
	}
	// "每日：" 仅是分隔标记，剥离后走原有的每日时段解析
	rest = dailyMarkerRegex.ReplaceAllString(rest, "")

	matches := timeRangeRegex.FindAllStringSubmatchIndex(rest, -1)
	if len(matches) == 0 {
		// 纯周级/纯日期范围条件（无每日时段）也构成有效时段
		if len(weekRanges) > 0 || dateRange != nil {
			label := strings.TrimRight(strings.TrimSpace(rest), "、，, ；;")
			label = strings.TrimLeft(label, "-—– ")
			if label == "" {
				return nil, fmt.Errorf("时段条件缺少标签: %q", cond)
			}
			return &timePeriodCond{Label: label, Timezone: tz, WeekRanges: weekRanges, DateRange: dateRange}, nil
		}
		return nil, nil
	}
	// 标签 = 首个时间范围之前的文字；范围之后只允许分隔符与空白
	label := strings.TrimSpace(rest[:matches[0][0]])
	label = strings.TrimRight(label, "、，, ；;")
	label = strings.TrimLeft(label, "-—– ")
	if label == "" {
		return nil, fmt.Errorf("时段条件缺少标签: %q", cond)
	}
	tail := strings.TrimSpace(rest[matches[len(matches)-1][1]:])
	if strings.Trim(tail, "、，, ；;") != "" {
		return nil, fmt.Errorf("时段条件包含无法识别的内容: %q", cond)
	}
	ranges := make([]minuteRange, 0, len(matches))
	for _, idx := range matches {
		m := timeRangeRegex.FindStringSubmatch(rest[idx[0]:idx[1]])
		startH, _ := strconv.Atoi(m[1])
		startM, _ := strconv.Atoi(m[2])
		nextDay := m[3] != ""
		endH, _ := strconv.Atoi(m[4])
		endM, _ := strconv.Atoi(m[5])
		if startH > 23 || endH > 23 || startM > 59 || endM > 59 {
			return nil, fmt.Errorf("时段超出合法时间: %q", rest[idx[0]:idx[1]])
		}
		start := startH*60 + startM
		end := endH*60 + endM
		if start == end {
			// "08:00-次日08:00" 这类全天时段不构成约束，忽略该范围
			if nextDay {
				continue
			}
			return nil, fmt.Errorf("时段起止时间相同: %q", rest[idx[0]:idx[1]])
		}
		if nextDay && end > start {
			return nil, fmt.Errorf("次日标记但结束时间晚于开始时间: %q", rest[idx[0]:idx[1]])
		}
		ranges = append(ranges, minuteRange{Start: start, End: end})
	}
	// 全部时段均为全天约束（或仅有标签）时视为无条件，归入默认组
	if len(ranges) == 0 && len(weekRanges) == 0 && dateRange == nil {
		return nil, nil
	}
	return &timePeriodCond{Label: label, Timezone: tz, Ranges: ranges, WeekRanges: weekRanges, DateRange: dateRange}, nil
}

// sameTimePeriod 判断两个时段条件的时区与范围是否一致（范围顺序无关）
func sameTimePeriod(a, b *timePeriodCond) bool {
	sameSet := func(x, y []minuteRange) bool {
		if len(x) != len(y) {
			return false
		}
		for _, rx := range x {
			found := false
			for _, ry := range y {
				if rx == ry {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	if a.Timezone != b.Timezone {
		return false
	}
	if !sameSet(a.Ranges, b.Ranges) || !sameSet(a.WeekRanges, b.WeekRanges) {
		return false
	}
	if (a.DateRange == nil) != (b.DateRange == nil) {
		return false
	}
	if a.DateRange != nil && *a.DateRange != *b.DateRange {
		return false
	}
	return true
}

// exprCondition 生成时段条件的表达式文本，按"分钟数"比较以兼容非整点。
// 单段：mins >= 540 && mins < 720；跨午夜：mins >= 1080 || mins < 480；多段以 || 连接。
func (tp *timePeriodCond) exprCondition() string {
	mins := fmt.Sprintf(`hour(%q) * 60 + minute(%q)`, tp.Timezone, tp.Timezone)
	parts := make([]string, 0, len(tp.Ranges)+len(tp.WeekRanges))
	for _, r := range tp.Ranges {
		if r.End > r.Start {
			parts = append(parts, fmt.Sprintf("%s >= %d && %s < %d", mins, r.Start, mins, r.End))
		} else {
			parts = append(parts, fmt.Sprintf("%s >= %d || %s < %d", mins, r.Start, mins, r.End))
		}
	}
	if len(tp.WeekRanges) > 0 {
		// 周分钟数：周一 00:00 为 0，周日为起点（weekday 周日=0）
		wm := fmt.Sprintf(`weekday(%q) * 1440 + %s`, tp.Timezone, mins)
		for _, r := range tp.WeekRanges {
			if r.End > r.Start {
				parts = append(parts, fmt.Sprintf("%s >= %d && %s < %d", wm, r.Start, wm, r.End))
			} else {
				parts = append(parts, fmt.Sprintf("%s >= %d || %s < %d", wm, r.Start, wm, r.End))
			}
		}
	}
	if tp.DateRange != nil {
		// 月日标量（月*100+日）比较起止日期，边界日期再比较当天分钟数
		md := fmt.Sprintf(`month(%q) * 100 + day(%q)`, tp.Timezone, tp.Timezone)
		dr := tp.DateRange
		dateCond := fmt.Sprintf("(%s > %d || %s == %d && %s >= %d) && (%s < %d || %s == %d && %s < %d)",
			md, dr.StartMD, md, dr.StartMD, mins, dr.StartMins,
			md, dr.EndMD, md, dr.EndMD, mins, dr.EndMins)
		if len(parts) > 0 {
			return dateCond + " && (" + strings.Join(parts, " || ") + ")"
		}
		return dateCond
	}
	return strings.Join(parts, " || ")
}

// formatPrice 输出最简十进制价格字符串，避免浮点尾巴（如 0.30000000000000004）
func formatPrice(price float64) string {
	return strconv.FormatFloat(price, 'f', -1, 64)
}

// discountDisplay 折扣系数的可读展示（0.6 → "6 折"，0.65 → "6.5 折"）
func discountDisplay(discount float64) string {
	zhe := math.Round(discount*100) / 10
	return formatPrice(zhe) + " 折"
}

// resolveModelDiscount 归并同一模型的折扣：取首个非 1 值；出现多个不同的非 1 折扣时
// 告警并保留首个。全部为空/1 时返回 1。
func resolveModelDiscount(modelID string, rows []csvPriceRow, ctx context.Context) float64 {
	discount := 1.0
	for _, r := range rows {
		if r.Discount == 1 {
			continue
		}
		if discount == 1 {
			discount = r.Discount
			continue
		}
		if discount != r.Discount {
			logger.LogWarn(ctx, fmt.Sprintf("模型 %s 存在不一致的折扣（%s 与 %s），按首个值 %s 处理",
				modelID, formatPrice(discount), formatPrice(r.Discount), formatPrice(discount)))
		}
	}
	return discount
}

// priceColumns 必需字段在表头中的列索引；discount 为可选列（-1 表示不存在）
type priceColumns struct {
	modelID  int
	desc     int
	price    int
	unit     int
	discount int
}

// maxIndex 必需列中的最大列索引，用于判断数据行列数是否足够
func (c priceColumns) maxIndex() int {
	m := c.modelID
	for _, i := range []int{c.desc, c.price, c.unit} {
		if i > m {
			m = i
		}
	}
	return m
}

// locatePriceColumns 按表头名称定位必需列，兼容 5 列 CSV 格式
// （模型ID/计费单元/说明/标价(元)/单位）与含附加列（模型介绍、厂商等）的 Excel 格式
func locatePriceColumns(header []string) (priceColumns, error) {
	cols := priceColumns{modelID: -1, desc: -1, price: -1, unit: -1, discount: -1}
	for i, h := range header {
		switch h = strings.TrimSpace(h); {
		case h == "模型ID":
			cols.modelID = i
		case h == "说明":
			cols.desc = i
		case strings.HasPrefix(h, "标价"):
			cols.price = i
		case h == "单位":
			cols.unit = i
		case h == "折扣":
			cols.discount = i
		}
	}
	if cols.modelID < 0 || cols.desc < 0 || cols.price < 0 || cols.unit < 0 {
		return cols, fmt.Errorf("表头缺少必需列（模型ID/说明/标价/单位），实际表头: %q", header)
	}
	return cols, nil
}

var errRecordTooShort = errors.New("记录列数不足")

// extractPriceRow 按列索引从一条数据记录提取价格行；
// 列数不足返回 errRecordTooShort，价格非法返回带原文的错误以便调用方记录日志。
// 折扣为可选列：缺列/空单元格按 1（不打折）；非法折扣（非数字、≤0、>1）不丢弃整行，
// 按 1 处理并通过 warn 返回说明，由调用方带行号记录日志。
func extractPriceRow(record []string, cols priceColumns) (row csvPriceRow, warn string, err error) {
	if len(record) <= cols.maxIndex() {
		return csvPriceRow{}, "", errRecordTooShort
	}
	price, err := strconv.ParseFloat(strings.TrimSpace(record[cols.price]), 64)
	if err != nil || !isValidNonNegativeCost(price) {
		return csvPriceRow{}, "", fmt.Errorf("价格无效: %q", record[cols.price])
	}
	discount := 1.0
	if cols.discount >= 0 && len(record) > cols.discount {
		if raw := strings.TrimSpace(record[cols.discount]); raw != "" {
			d, dErr := strconv.ParseFloat(raw, 64)
			if dErr != nil || d <= 0 || d > 1 {
				warn = fmt.Sprintf("折扣无效: %q，按 1（不打折）处理", raw)
			} else {
				discount = d
			}
		}
	}
	return csvPriceRow{
		ModelID:  strings.TrimSpace(record[cols.modelID]),
		Desc:     strings.TrimSpace(record[cols.desc]),
		Price:    price,
		Unit:     strings.TrimSpace(record[cols.unit]),
		Discount: discount,
	}, warn, nil
}

// parseCSVRows 解析 CSV 全部数据行，自动跳过 UTF-8 BOM 与表头。
// 第二个返回值表示表头中是否存在折扣列（无折扣列的文件不应执行折扣覆盖语义）。
func parseCSVRows(reader io.Reader, ctx context.Context) ([]csvPriceRow, bool, error) {
	br := bufio.NewReader(reader)
	if bom, err := br.Peek(3); err == nil && len(bom) == 3 && bom[0] == 0xEF && bom[1] == 0xBB && bom[2] == 0xBF {
		_, _ = br.Discard(3)
	}

	r := csv.NewReader(br)
	r.LazyQuotes = true
	r.TrimLeadingSpace = true

	header, err := r.Read()
	if err != nil {
		return nil, false, fmt.Errorf("读取 CSV 表头失败: %w", err)
	}
	cols, err := locatePriceColumns(header)
	if err != nil {
		return nil, false, err
	}

	var rows []csvPriceRow
	lineNum := 1
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		lineNum++
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("CSV 第 %d 行解析失败: %v", lineNum, err))
			continue
		}
		row, warn, err := extractPriceRow(record, cols)
		if errors.Is(err, errRecordTooShort) {
			continue
		}
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("CSV 第 %d 行%v", lineNum, err))
			continue
		}
		if warn != "" {
			logger.LogWarn(ctx, fmt.Sprintf("CSV 第 %d 行%s", lineNum, warn))
		}
		rows = append(rows, row)
	}
	return rows, cols.discount >= 0, nil
}

// parseXLSXRows 解析 Excel(.xlsx) 第一个工作表的全部数据行，首行为表头。
// 必需列（模型ID/说明/标价/单位）按表头名称定位，允许存在其他附加列。
// 第二个返回值表示表头中是否存在折扣列。
func parseXLSXRows(reader io.Reader, ctx context.Context) ([]csvPriceRow, bool, error) {
	f, err := excelize.OpenReader(reader)
	if err != nil {
		return nil, false, fmt.Errorf("读取 Excel 文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, false, fmt.Errorf("Excel 文件中没有工作表")
	}
	records, err := f.GetRows(sheets[0])
	if err != nil {
		return nil, false, fmt.Errorf("读取 Excel 工作表失败: %w", err)
	}
	if len(records) == 0 {
		return nil, false, fmt.Errorf("Excel 文件为空")
	}
	cols, err := locatePriceColumns(records[0])
	if err != nil {
		return nil, false, err
	}

	var rows []csvPriceRow
	for i, record := range records[1:] {
		row, warn, err := extractPriceRow(record, cols)
		if errors.Is(err, errRecordTooShort) {
			continue
		}
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("Excel 第 %d 行%v", i+2, err))
			continue
		}
		if warn != "" {
			logger.LogWarn(ctx, fmt.Sprintf("Excel 第 %d 行%s", i+2, warn))
		}
		rows = append(rows, row)
	}
	return rows, cols.discount >= 0, nil
}

// csvConvertResult 转换结果的分类收集器
type csvConvertResult struct {
	modelPrices  map[string]float64
	modelRatios  map[string]float64 // 折扣系数（≠1 时写入，复用 ModelRatio 折扣结算位）
	billingModes map[string]string
	billingExprs map[string]string
	// displayPrices 每个模型各同步字段的可读价格展示内容，供差异对比界面直接展示：
	// 简单字段为 string（如 "4.2 元/M"），billing_expr 为 []displayPriceLine（逐变量价格行）
	displayPrices map[string]map[string]any
	// skipReasons 每个模型被跳过的价格行原因（"code: 原文"），供前端提示解析缺口
	skipReasons map[string][]string
}

func newCSVConvertResult() *csvConvertResult {
	return &csvConvertResult{
		modelPrices:  make(map[string]float64),
		modelRatios:  make(map[string]float64),
		billingModes: make(map[string]string),
		billingExprs: make(map[string]string),
		displayPrices: make(map[string]map[string]any),
		skipReasons:  make(map[string][]string),
	}
}

// maxSkipReasonsPerModel 每个模型保留的跳过原因上限，避免异常文件撑爆响应
const maxSkipReasonsPerModel = 5

// reasonNoPricingProduced 摘要原因：模型有价格行但未产出任何计费规则
const reasonNoPricingProduced = "no_pricing_produced"

// addSkipReason 记录某模型一行被跳过的原因（重复原因去重，超上限截断；
// 摘要原因 no_pricing_produced 不受上限约束，确保零产出模型必定被上报）
func (r *csvConvertResult) addSkipReason(modelID, reason string) {
	reasons := r.skipReasons[modelID]
	for _, existing := range reasons {
		if existing == reason {
			return
		}
	}
	if len(reasons) >= maxSkipReasonsPerModel && reason != reasonNoPricingProduced {
		return
	}
	r.skipReasons[modelID] = append(reasons, reason)
}

// modelParseIssue 一个模型的解析缺口：哪些价格行被跳过及原因
type modelParseIssue struct {
	Model   string   `json:"model"`
	Reasons []string `json:"reasons"`
}

// setDisplayPrice 记录某模型某字段的可读价格展示内容
func (r *csvConvertResult) setDisplayPrice(modelID, field string, content any) {
	dp, ok := r.displayPrices[modelID]
	if !ok {
		dp = make(map[string]any)
		r.displayPrices[modelID] = dp
	}
	dp[field] = content
}

// addTierPrice 记录变量某档位价格；同档位重复出现时保留最高价。
// 组合条件行（service_tier/is_batch/输出长度/分辨率等无法按段计费的维度）折叠进
// 可解析的输入长度档位后与标准行同档并存，取最高价即"按最高档计费"（保守防亏损），
// 且与文件中行的先后顺序无关
func addTierPrice(tiers map[string][]priceTier, varName string, bound int64, price float64) {
	list := tiers[varName]
	for i := range list {
		if list[i].UpperBound == bound {
			if price > list[i].Price {
				list[i].Price = price
			}
			return
		}
	}
	tiers[varName] = append(list, priceTier{UpperBound: bound, Price: price})
}

// singlePrice 取变量单档价格（无阶梯场景）
func singlePrice(list []priceTier) float64 {
	if len(list) == 0 {
		return 0
	}
	return list[0].Price
}

// collectTierBounds 收集所有变量的阶梯上限，去重升序
func collectTierBounds(tiers map[string][]priceTier) []int64 {
	boundSet := make(map[int64]bool)
	for _, list := range tiers {
		for _, t := range list {
			if t.UpperBound > 0 {
				boundSet[t.UpperBound] = true
			}
		}
	}
	bounds := make([]int64, 0, len(boundSet))
	for b := range boundSet {
		bounds = append(bounds, b)
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })
	return bounds
}

// priceForBound 取变量在指定档位的价格：优先精确匹配档位上限，其次回退默认档
func priceForBound(list []priceTier, bound int64) (float64, bool) {
	for _, t := range list {
		if t.UpperBound == bound {
			return t.Price, true
		}
	}
	for _, t := range list {
		if t.UpperBound == 0 {
			return t.Price, true
		}
	}
	return 0, false
}

// buildExprSum 生成某一档位的表达式求和部分，如 "p*4.2 + c*16.8 + cr*0.84"
// bound 为 0 表示单档（取各变量默认档或唯一档价格）
func buildExprSum(tiers map[string][]priceTier, bound int64) string {
	parts := make([]string, 0, len(tiers))
	for _, v := range exprVarOrder {
		list, ok := tiers[v]
		if !ok {
			continue
		}
		var price float64
		var found bool
		if bound == 0 {
			price, found = priceForBound(list, 0)
			if !found && len(list) == 1 {
				price, found = list[0].Price, true
			}
		} else {
			price, found = priceForBound(list, bound)
		}
		if found {
			parts = append(parts, fmt.Sprintf("%s*%s", v, formatPrice(price)))
		}
	}
	return strings.Join(parts, " + ")
}

// tierNames 档位命名：两档用语义名，三档及以上用序号
func tierNames(n int) []string {
	if n == 2 {
		return []string{"standard", "long_context"}
	}
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("tier_%d", i+1)
	}
	return names
}

// buildTieredExpression 由按变量归类的档位价格生成完整计费表达式。
// 无阶梯时输出简单求和；有阶梯时生成嵌套三元，末档为 else 分支。
func buildTieredExpression(tiers map[string][]priceTier) string {
	return buildTieredExpressionNamed(tiers, "")
}

// buildTieredExpressionNamed 与 buildTieredExpression 相同，但 namePrefix 非空时：
// 无阶梯求和会包一层 tier(namePrefix, ...) 以保留时段标签；有阶梯时档位名加此前缀，
// 用于时段分组表达式中区分各档位所属的时段。
func buildTieredExpressionNamed(tiers map[string][]priceTier, namePrefix string) string {
	bounds := collectTierBounds(tiers)
	if len(bounds) == 0 {
		sum := buildExprSum(tiers, 0)
		if namePrefix != "" {
			return fmt.Sprintf("tier(%q, %s)", namePrefix, sum)
		}
		return sum
	}
	names := tierNames(len(bounds))
	if namePrefix != "" {
		for i := range names {
			names[i] = namePrefix + " " + names[i]
		}
	}
	last := len(bounds) - 1
	expr := fmt.Sprintf("tier(%q, %s)", names[last], buildExprSum(tiers, bounds[last]))
	for i := last - 1; i >= 0; i-- {
		expr = fmt.Sprintf("len <= %d ? tier(%q, %s) : (%s)", bounds[i], names[i], buildExprSum(tiers, bounds[i]), expr)
	}
	return expr
}

// buildExprDisplayLines 由按变量归类的档位价格生成结构化可读价格行，每个变量一条，
// 如 {label: "Input price", value: "4.2/8.4 元/M"}（多档价格按档位升序以 / 分隔，同价档位合并）
func buildExprDisplayLines(tiers map[string][]priceTier) []displayPriceLine {
	bounds := collectTierBounds(tiers)
	lines := make([]displayPriceLine, 0, len(tiers))
	for _, v := range exprVarOrder {
		list, ok := tiers[v]
		if !ok {
			continue
		}
		label := exprVarDisplayLabel[v]
		if len(bounds) == 0 {
			lines = append(lines, displayPriceLine{
				Label: label,
				Value: fmt.Sprintf("%s 元/M", formatPrice(singlePrice(list))),
			})
			continue
		}
		prices := make([]string, 0, len(bounds))
		seen := make(map[string]bool)
		for _, b := range bounds {
			if p, found := priceForBound(list, b); found {
				s := formatPrice(p)
				if !seen[s] {
					seen[s] = true
					prices = append(prices, s)
				}
			}
		}
		lines = append(lines, displayPriceLine{
			Label: label,
			Value: fmt.Sprintf("%s 元/M", strings.Join(prices, "/")),
		})
	}
	return lines
}

// periodGroup 同一时段条件下按变量归类的档位价格；period 为 nil 表示无时段条件的默认组
type periodGroup struct {
	period *timePeriodCond
	tiers  map[string][]priceTier
}

// buildTimeSlicedExpression 由时段分组生成时间条件嵌套三元表达式。
// 兜底（else）分支：存在默认组（无时段条件）时为默认组，否则为最后出现的时段组（如谷时）。
func buildTimeSlicedExpression(groups []*periodGroup) string {
	fallbackIdx := len(groups) - 1
	for i, g := range groups {
		if g.period == nil {
			fallbackIdx = i
			break
		}
	}
	body := func(g *periodGroup) string {
		if g.period == nil {
			return buildTieredExpression(g.tiers)
		}
		return buildTieredExpressionNamed(g.tiers, g.period.Label)
	}
	expr := body(groups[fallbackIdx])
	for i := len(groups) - 1; i >= 0; i-- {
		if i == fallbackIdx {
			continue
		}
		thenBody := body(groups[i])
		if strings.Contains(thenBody, " ? ") {
			thenBody = "(" + thenBody + ")"
		}
		expr = fmt.Sprintf("%s ? %s : (%s)", groups[i].period.exprCondition(), thenBody, expr)
	}
	return expr
}

// buildPeriodDisplayLines 为多时段分组生成可读价格行，时段组的价格行标签带时段后缀，
// 如 {label: "Input price (峰时)", value: "3 元/M"}
func buildPeriodDisplayLines(groups []*periodGroup) []displayPriceLine {
	var lines []displayPriceLine
	for _, g := range groups {
		for _, l := range buildExprDisplayLines(g.tiers) {
			if g.period != nil {
				l.Label = fmt.Sprintf("%s (%s)", l.Label, g.period.Label)
			}
			lines = append(lines, l)
		}
	}
	return lines
}

// buildTokenPricing 处理 token 类模型：简单模型出倍率，阶梯/多模态/含 cc1h 模型出表达式，
// 含峰谷时段条件的模型出时间条件嵌套三元表达式。
// discount ≠1 时写入 modelRatio 折扣位（结算时表达式成本 × 折扣），表达式系数保持标价。
func buildTokenPricing(modelID string, rows []csvPriceRow, discount float64, result *csvConvertResult, ctx context.Context) {
	var groups []*periodGroup
	groupByLabel := make(map[string]*periodGroup)
	var defaultGroup *periodGroup

	for _, r := range rows {
		typ, cond := parseDesc(r.Desc)
		if csvDescTypeSkip[typ] {
			continue
		}
		varName, ok := csvDescTypeToVar[typ]
		if !ok {
			logger.LogWarn(ctx, fmt.Sprintf("模型 %s 存在未识别的说明类型: %q", modelID, typ))
			result.addSkipReason(modelID, "unknown_desc_type: "+r.Desc)
			continue
		}
		bound := parseTierUpperBound(cond)
		// 时段与长度阶梯可能同时出现在条件中，先剥离阶梯部分再解析时段
		strippedCond := tierBoundRegex.ReplaceAllString(cond, "")
		// "且 service_tier=Priority" 这类组合请求条件（按请求头/体/输出长度/分辨率等
		// 区分价格）无法按段计费：折叠进可解析的输入长度档位（无档位则进默认档），
		// 同档位取最高价（统一按最高档计费），不再整行跳过
		if strings.Contains(strippedCond, "且 ") {
			logger.LogWarn(ctx, fmt.Sprintf("模型 %s 的组合条件无法按段计费，已按最高档价格计费 %q", modelID, r.Desc))
			result.addSkipReason(modelID, "highest_tier_fallback: "+r.Desc)
			if defaultGroup == nil {
				defaultGroup = &periodGroup{tiers: make(map[string][]priceTier)}
				groups = append(groups, defaultGroup)
			}
			addTierPrice(defaultGroup.tiers, varName, bound, r.Price)
			continue
		}
		period, err := parseTimePeriodCondition(strippedCond)
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("模型 %s 时段条件无效，已跳过 %q: %v", modelID, r.Desc, err))
			result.addSkipReason(modelID, "invalid_time_period: "+r.Desc)
			continue
		}
		var g *periodGroup
		if period == nil {
			if defaultGroup == nil {
				defaultGroup = &periodGroup{tiers: make(map[string][]priceTier)}
				groups = append(groups, defaultGroup)
			}
			g = defaultGroup
		} else {
			g = groupByLabel[period.Label]
			if g != nil {
				if !sameTimePeriod(g.period, period) {
					logger.LogWarn(ctx, fmt.Sprintf("模型 %s 时段标签 %q 的时间范围不一致，已跳过 %q", modelID, period.Label, r.Desc))
					result.addSkipReason(modelID, "time_label_mismatch: "+r.Desc)
					continue
				}
			} else {
				g = &periodGroup{period: period, tiers: make(map[string][]priceTier)}
				groupByLabel[period.Label] = g
				groups = append(groups, g)
			}
		}
		addTierPrice(g.tiers, varName, bound, r.Price)
	}

	if len(groups) == 0 {
		return
	}

	// 统一走直接单价表达式：p*输入价 + c*输出价 + cr*缓存价 + cc*写缓存价
	// 表达式系数即 CSV 人民币价格，不再走 model_ratio/completion_ratio 倍率换算
	var expr string
	var displayLines []displayPriceLine
	if len(groups) == 1 && groups[0].period == nil {
		expr = buildTieredExpression(groups[0].tiers)
		displayLines = buildExprDisplayLines(groups[0].tiers)
	} else {
		expr = buildTimeSlicedExpression(groups)
		displayLines = buildPeriodDisplayLines(groups)
	}
	if _, err := billingexpr.CompileFromCache(expr); err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("模型 %s 表达式编译失败，已跳过: %v", modelID, err))
		result.addSkipReason(modelID, "expr_compile_failed: "+expr)
		return
	}
	if err := billing_setting.SmokeTestExpr(expr); err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("模型 %s 表达式冒烟测试失败，已跳过: %v", modelID, err))
		result.addSkipReason(modelID, "expr_compile_failed: "+err.Error())
		return
	}
	result.billingModes[modelID] = billing_setting.BillingModeTieredExpr
	result.billingExprs[modelID] = expr
	result.setDisplayPrice(modelID, "billing_mode", "Expression billing")
	result.setDisplayPrice(modelID, "billing_expr", displayLines)
	if discount != 1 {
		result.modelRatios[modelID] = discount
		result.setDisplayPrice(modelID, "model_ratio", discountDisplay(discount))
	}
}

// selectRepresentativePrice 代表价选择：默认档优先，无默认取最高价（保守防亏损）
func selectRepresentativePrice(candidates []csvPriceRow) float64 {
	for _, c := range candidates {
		_, cond := parseDesc(c.Desc)
		if cond == "默认" {
			return c.Price
		}
	}
	maxPrice := 0.0
	for _, c := range candidates {
		if c.Price > maxPrice {
			maxPrice = c.Price
		}
	}
	return maxPrice
}

// buildFixedPricing 处理非 token 类模型：按单位优先级选代表价，生成 model_price。
// ModelRatio 折扣机制仅作用于 tiered_expr 结算，固定价模型折扣直接折进 model_price。
func buildFixedPricing(modelID string, rows []csvPriceRow, discount float64, result *csvConvertResult) {
	byUnit := make(map[string][]csvPriceRow)
	for _, r := range rows {
		byUnit[r.Unit] = append(byUnit[r.Unit], r)
	}
	for _, unit := range fixedUnitPriority {
		candidates, ok := byUnit[unit]
		if !ok {
			continue
		}
		price := selectRepresentativePrice(candidates) * discount
		result.modelPrices[modelID] = roundRatioValue(price)
		result.setDisplayPrice(modelID, "model_price", fmt.Sprintf("%s 元/%s", formatPrice(price), unit))
		return
	}
}

// buildModelPricing 按模型分发：token 类优先，纯非 token 类走固定价
func buildModelPricing(modelID string, rows []csvPriceRow, result *csvConvertResult, ctx context.Context) {
	var tokenRows, fixedRows []csvPriceRow
	for _, r := range rows {
		switch {
		case r.Unit == "百万 Token":
			tokenRows = append(tokenRows, r)
		case r.Unit == "":
			// 空单位（gemini 缓存存储等）跳过
		default:
			fixedRows = append(fixedRows, r)
		}
	}

	discount := resolveModelDiscount(modelID, rows, ctx)
	if len(tokenRows) > 0 {
		buildTokenPricing(modelID, tokenRows, discount, result, ctx)
	} else if len(fixedRows) > 0 {
		buildFixedPricing(modelID, fixedRows, discount, result)
	}
}

// convertCSVToRatioData 将 CSV 内容转换为同步数据格式，仅保留 localModels 中存在的模型
func convertCSVToRatioData(reader io.Reader, localModels map[string]bool, ctx context.Context) (map[string]any, map[string]map[string]any, []string, []modelParseIssue, error) {
	rows, _, err := parseCSVRows(reader, ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return convertRowsToRatioData(rows, localModels, ctx)
}

// convertRowsToRatioData 将解析出的价格行转换为同步数据格式，仅保留 localModels 中存在的模型。
// 返回的 map key 与 pricingSyncFields 对齐，可直接交给 buildDifferences 复用；
// 同时返回每个模型各字段的可读价格展示内容，供差异对比界面展示；
// parseIssues 返回存在被跳过价格行的模型及原因（解析缺口，前端据此提示）。
func convertRowsToRatioData(rows []csvPriceRow, localModels map[string]bool, ctx context.Context) (map[string]any, map[string]map[string]any, []string, []modelParseIssue, error) {
	if len(rows) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("文件中无有效数据行")
	}

	modelRows := make(map[string][]csvPriceRow)
	for _, r := range rows {
		if r.ModelID == "" {
			continue
		}
		modelRows[r.ModelID] = append(modelRows[r.ModelID], r)
	}

	result := newCSVConvertResult()
	var skippedModels []string

	for modelID, mRows := range modelRows {
		if !localModels[modelID] {
			skippedModels = append(skippedModels, modelID)
			continue
		}
		buildModelPricing(modelID, mRows, result, ctx)
		// 有价格行却未产出任何计费规则（全部行被跳过或单位无法识别）时，
		// 显式记录解析缺口，避免模型悄无声息地从导入结果中消失
		if _, hasMode := result.billingModes[modelID]; !hasMode {
			if _, hasPrice := result.modelPrices[modelID]; !hasPrice {
				result.addSkipReason(modelID, reasonNoPricingProduced)
			}
		}
	}
	sort.Strings(skippedModels)

	parseIssues := make([]modelParseIssue, 0, len(result.skipReasons))
	for modelID := range modelRows {
		if !localModels[modelID] {
			continue
		}
		if reasons := result.skipReasons[modelID]; len(reasons) > 0 {
			parseIssues = append(parseIssues, modelParseIssue{Model: modelID, Reasons: reasons})
		}
	}
	sort.Slice(parseIssues, func(i, j int) bool { return parseIssues[i].Model < parseIssues[j].Model })

	converted := make(map[string]any)
	if len(result.modelPrices) > 0 {
		converted["model_price"] = result.modelPrices
	}
	if len(result.modelRatios) > 0 {
		converted["model_ratio"] = result.modelRatios
	}
	if len(result.billingModes) > 0 {
		converted[billing_setting.BillingModeField] = result.billingModes
	}
	if len(result.billingExprs) > 0 {
		converted[billing_setting.BillingExprField] = result.billingExprs
	}
	return converted, result.displayPrices, skippedModels, parseIssues, nil
}

// injectDiscountClearRows 折扣列语义为全量覆盖：1 表示无折扣。
// buildTokenPricing 只在折扣 ≠1 时产出 model_ratio，因此本地已配置折扣（≠1）
// 而 CSV 折扣为 1 的 token 类模型不会产生差异行，旧折扣永远清不掉。
// 这里为这类模型显式补一条 model_ratio=1 的覆盖项，使同步能按表清除旧折扣。
// 仅限文件表头带折扣列时由调用方触发；且只处理本地已是表达式计费的模型——
// 倍率计费模型的 model_ratio 是基础倍率（含内置默认表），不是折扣，清成 1 会破坏定价。
func injectDiscountClearRows(converted map[string]any, localData map[string]any) {
	modes, ok := converted[billing_setting.BillingModeField].(map[string]string)
	if !ok || len(modes) == 0 {
		return
	}
	localRatios := valueMap(localData["model_ratio"])
	localModes := valueMap(localData[billing_setting.BillingModeField])
	defaultRatios := ratio_setting.GetDefaultModelRatioMap()
	var ratios map[string]float64
	if existing, ok := converted["model_ratio"].(map[string]float64); ok {
		ratios = existing
	}
	for modelID := range modes {
		if _, has := ratios[modelID]; has {
			continue // 折扣 ≠1 已正常产出
		}
		if mode, _ := localModes[modelID].(string); mode != billing_setting.BillingModeTieredExpr {
			continue // 本地非表达式计费：model_ratio 是基础倍率而非折扣
		}
		local, exists := localRatios[modelID]
		if !exists {
			continue
		}
		f, ok := asFloat64(local)
		if !ok || nearlyEqual(f, 1) {
			continue
		}
		// 与 GetTieredModelRatioDiscount 同口径：等于内置默认表的基础倍率不算折扣
		if defaultRatio, hasDefault := defaultRatios[modelID]; hasDefault && nearlyEqual(defaultRatio, f) {
			continue
		}
		if ratios == nil {
			ratios = make(map[string]float64)
			converted["model_ratio"] = ratios
		}
		ratios[modelID] = 1
	}
}

// FetchCSVUpstreamRatios 接收上传的 CSV/Excel 文件，解析后复用差异对比流程返回结果
func FetchCSVUpstreamRatios(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "请选择要上传的 CSV 或 Excel 文件"})
		return
	}
	if fileHeader.Size > maxCSVFileSize {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "文件大小不能超过 10MB"})
		return
	}
	filename := strings.ToLower(fileHeader.Filename)
	isCSV := strings.HasSuffix(filename, ".csv")
	isXLSX := strings.HasSuffix(filename, ".xlsx")
	if !isCSV && !isXLSX {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "仅支持 .csv 或 .xlsx 文件"})
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		logger.LogError(c.Request.Context(), "打开上传文件失败: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "无法读取上传文件"})
		return
	}
	defer file.Close()

	localData := getLocalPricingSyncData()
	// 匹配基准 = 启用渠道中的模型 ∪ 本地已配置价格的模型。
	// 本地未配置价格但渠道中存在的模型允许导入（作为新增定价），
	// 只有渠道中完全不存在的模型才会被跳过。
	localModels := make(map[string]bool)
	for _, field := range pricingSyncFields {
		for name := range valueMap(localData[field]) {
			localModels[name] = true
		}
	}
	channels, err := model.GetAllChannels(0, 0, true, false)
	if err != nil {
		logger.LogError(c.Request.Context(), "查询渠道列表失败，仅按本地定价配置匹配: "+err.Error())
	} else {
		for _, channel := range channels {
			if channel.Status != common.ChannelStatusEnabled {
				continue
			}
			for _, m := range channel.GetModels() {
				if m = strings.TrimSpace(m); m != "" {
					localModels[m] = true
				}
			}
		}
	}

	var rows []csvPriceRow
	discountColPresent := false
	if isXLSX {
		rows, discountColPresent, err = parseXLSXRows(file, c.Request.Context())
	} else {
		rows, discountColPresent, err = parseCSVRows(file, c.Request.Context())
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "文件解析失败: " + err.Error()})
		return
	}

	converted, displayPrices, skippedModels, parseIssues, err := convertRowsToRatioData(rows, localModels, c.Request.Context())
	// 折扣覆盖语义仅在文件明确带折扣列时生效：无折扣列的文件不应清除已有折扣
	if err == nil && discountColPresent {
		injectDiscountClearRows(converted, localData)
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "文件解析失败: " + err.Error()})
		return
	}

	successfulChannels := []struct {
		name string
		data map[string]any
	}{{name: csvImportChannelName, data: converted}}
	differences := buildDifferences(localData, successfulChannels)

	logger.LogInfo(c.Request.Context(), fmt.Sprintf("定价文件导入解析完成: 跳过 %d 个渠道中不存在的模型", len(skippedModels)))

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"differences": differences,
			"test_results": []dto.TestResult{{
				Name:   csvImportChannelName,
				Status: "success",
			}},
			"skipped_models": skippedModels,
			"display_prices": displayPrices,
			"parse_issues":   parseIssues,
		},
	})
}
