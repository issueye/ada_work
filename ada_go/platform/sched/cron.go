package sched

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
)

// Cron 是解析后的 5 段 cron 表达式(分 时 日 月 周)。
//
// 只支持「纯计算」的语法:不支持秒、不支持 @every/@reboot(那些用 Every/After 表达),
// 也不支持时区后缀——时区由传入的 time.Location 决定。
type Cron struct {
	minutes map[int]bool
	hours   map[int]bool
	doms    map[int]bool
	months  map[int]bool
	dows    map[int]bool
	domStar bool
	dowStar bool
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dowNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// ParseCron 解析 5 段表达式。
//
// 支持:*、*/step、a、a-b、a-b/step、列表(逗号)、月份/星期的英文缩写。
// 超出范围的值、负数与空段一律报 EINVALID,不做「尽量理解」。
func ParseCron(spec string) (*Cron, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, kernel.Fail(kernel.CodeInvalid,
			"sched: cron 表达式需要 5 段(分 时 日 月 周),得到 %d 段: %q", len(fields), spec)
	}
	minutes, _, err := parseCronField(fields[0], 0, 59, nil)
	if err != nil {
		return nil, err
	}
	hours, _, err := parseCronField(fields[1], 0, 23, nil)
	if err != nil {
		return nil, err
	}
	doms, domStar, err := parseCronField(fields[2], 1, 31, nil)
	if err != nil {
		return nil, err
	}
	months, _, err := parseCronField(fields[3], 1, 12, monthNames)
	if err != nil {
		return nil, err
	}
	dows, dowStar, err := parseCronField(fields[4], 0, 7, dowNames)
	if err != nil {
		return nil, err
	}
	// 7 与 0 都是周日。
	if dows[7] {
		dows[0] = true
		delete(dows, 7)
	}
	return &Cron{
		minutes: minutes, hours: hours, doms: doms, months: months, dows: dows,
		domStar: domStar, dowStar: dowStar,
	}, nil
}

func parseCronField(field string, min, max int, names map[string]int) (map[int]bool, bool, error) {
	set := make(map[int]bool)
	star := false
	for _, raw := range strings.Split(field, ",") {
		part := strings.TrimSpace(raw)
		if part == "" {
			return nil, false, kernel.Fail(kernel.CodeInvalid, "sched: cron 段里有空项: %q", field)
		}
		base := part
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			base = part[:i]
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return nil, false, kernel.Fail(kernel.CodeInvalid,
					"sched: cron 步长非法: %q", part)
			}
			step = n
		}

		switch {
		case base == "*":
			star = true
			for v := min; v <= max; v += step {
				set[v] = true
			}
		case strings.Contains(base, "-"):
			lo, hi, err := parseCronRange(base, names)
			if err != nil {
				return nil, false, err
			}
			if lo < min || hi > max || lo > hi {
				return nil, false, kernel.Fail(kernel.CodeInvalid,
					"sched: cron 范围越界: %q(%d..%d,允许 %d..%d)", base, lo, hi, min, max)
			}
			for v := lo; v <= hi; v += step {
				set[v] = true
			}
		default:
			v, err := parseCronValue(base, names)
			if err != nil {
				return nil, false, err
			}
			if v < min || v > max {
				return nil, false, kernel.Fail(kernel.CodeInvalid,
					"sched: cron 取值越界: %q(%d,允许 %d..%d)", base, v, min, max)
			}
			if step > 1 {
				// `5/10` 的语义:从 5 开始、到上界、步长 10。
				for x := v; x <= max; x += step {
					set[x] = true
				}
			} else {
				set[v] = true
			}
		}
	}
	return set, star, nil
}

func parseCronRange(base string, names map[string]int) (int, int, error) {
	parts := strings.SplitN(base, "-", 2)
	lo, err := parseCronValue(parts[0], names)
	if err != nil {
		return 0, 0, err
	}
	hi, err := parseCronValue(parts[1], names)
	if err != nil {
		return 0, 0, err
	}
	return lo, hi, nil
}

func parseCronValue(s string, names map[string]int) (int, error) {
	if names != nil {
		if v, ok := names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, kernel.Fail(kernel.CodeInvalid, "sched: cron 值不是数字: %q", s)
	}
	return v, nil
}

// Next 返回 after 之后(不含 after 所在的那一分钟)的第一个触发时间。
// 5 年内无解返回 false(例如 2 月 30 日这种永远不存在的日期)。
func (c *Cron) Next(after time.Time) (time.Time, bool) {
	loc := after.Location()
	floor := after.Truncate(time.Minute).Add(time.Minute)
	cur := time.Date(floor.Year(), floor.Month(), floor.Day(), 0, 0, 0, 0, loc)
	for i := 0; i < 366*5; i++ {
		y, mo, d := cur.Date()
		if !c.months[int(mo)] {
			cur = time.Date(y, mo+1, 1, 0, 0, 0, 0, loc)
			floor = cur
			continue
		}
		if !c.dayMatches(d, int(cur.Weekday())) {
			cur = cur.AddDate(0, 0, 1)
			floor = cur
			continue
		}
		h0, m0 := 0, 0
		if sameDay(cur, floor) {
			h0, m0 = floor.Hour(), floor.Minute()
		}
		for h := h0; h < 24; h++ {
			if !c.hours[h] {
				continue
			}
			startMinute := 0
			if h == h0 {
				startMinute = m0
			}
			for m := startMinute; m < 60; m++ {
				if !c.minutes[m] {
					continue
				}
				cand := time.Date(cur.Year(), cur.Month(), cur.Day(), h, m, 0, 0, loc)
				if !cand.Before(floor) {
					return cand, true
				}
			}
		}
		cur = cur.AddDate(0, 0, 1)
		floor = cur
	}
	return time.Time{}, false
}

func (c *Cron) dayMatches(dom, dow int) bool {
	domOK := c.domStar || c.doms[dom]
	dowOK := c.dowStar || c.dows[dow]
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	default:
		// 两段都受限时,标准 cron 语义是「任一满足」。
		return domOK || dowOK
	}
}

func sameDay(a, b time.Time) bool {
	y1, m1, d1 := a.Date()
	y2, m2, d2 := b.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// String 便于日志输出。
func (c *Cron) String() string {
	return fmt.Sprintf("cron(min=%v hour=%v dom=%v mon=%v dow=%v)", c.minutes, c.hours, c.doms, c.months, c.dows)
}
