// Package version 是 semver 解析与约束比较(L2 基础设施)。
//
// 够用就好:只支持版本号 + 预发布段,与约束 `>= > <= < = !=`(裸版本等价于 `=`)。
// 不支持范围并集、^/~ 与通配——出现这些形式一律报错,不做「尽量理解」。
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// Version 是一个语义化版本号。
type Version struct {
	Major, Minor, Patch int
	Pre                 string // 预发布段(不含 '-' 前缀)
}

// Parse 解析版本号;允许前导 "v"。
func Parse(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	raw = strings.TrimPrefix(raw, "v")
	if raw == "" {
		return Version{}, fmt.Errorf("version: 版本号不能为空")
	}
	var v Version
	main := raw
	if i := strings.IndexByte(raw, '-'); i >= 0 {
		main = raw[:i]
		v.Pre = raw[i+1:]
		if v.Pre == "" {
			return Version{}, fmt.Errorf("version: 预发布段为空: %q", s)
		}
	}
	parts := strings.Split(main, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version: 需要 major.minor.patch,得到 %q", s)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("version: 非法数字段 %q", p)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// String 返回规范形式。
func (v Version) String() string {
	out := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		out += "-" + v.Pre
	}
	return out
}

// Compare 比较两个版本:小于返回 -1,等于 0,大于 1。
func Compare(a, b Version) int {
	if a.Major != b.Major {
		return cmpInt(a.Major, b.Major)
	}
	if a.Minor != b.Minor {
		return cmpInt(a.Minor, b.Minor)
	}
	if a.Patch != b.Patch {
		return cmpInt(a.Patch, b.Patch)
	}
	return comparePre(a.Pre, b.Pre)
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// comparePre 按 semver:没有预发布段 > 有预发布段;否则逐段比较。
func comparePre(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1
	}
	if b == "" {
		return -1
	}
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		an, aErr := strconv.Atoi(as[i])
		bn, bErr := strconv.Atoi(bs[i])
		switch {
		case aErr == nil && bErr == nil:
			return cmpInt(an, bn)
		case aErr == nil:
			return -1 // 数字段 < 字符串段
		case bErr == nil:
			return 1
		default:
			if as[i] < bs[i] {
				return -1
			}
			return 1
		}
	}
	return cmpInt(len(as), len(bs))
}

// Constraint 是单条版本约束。
type Constraint struct {
	Op      string
	Version Version
}

// ParseConstraint 解析约束,如 ">=2.0.0"、"> 1.2.3"、"="、"2.0.0"。
func ParseConstraint(s string) (Constraint, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Constraint{}, fmt.Errorf("version: 约束不能为空")
	}
	op := "="
	for _, candidate := range []string{">=", "<=", "==", "!=", ">", "<", "="} {
		if strings.HasPrefix(raw, candidate) {
			op = candidate
			raw = strings.TrimSpace(raw[len(candidate):])
			break
		}
	}
	v, err := Parse(raw)
	if err != nil {
		return Constraint{}, fmt.Errorf("version: 约束 %q 非法: %w", s, err)
	}
	return Constraint{Op: op, Version: v}, nil
}

// Check 判断版本是否满足约束。
func (c Constraint) Check(v Version) bool {
	cmp := Compare(v, c.Version)
	switch c.Op {
	case ">=":
		return cmp >= 0
	case ">":
		return cmp > 0
	case "<=":
		return cmp <= 0
	case "<":
		return cmp < 0
	case "=", "==":
		return cmp == 0
	case "!=":
		return cmp != 0
	}
	return false
}

// CheckString 解析 host 后判断约束。
func (c Constraint) CheckString(host string) (bool, error) {
	v, err := Parse(host)
	if err != nil {
		return false, err
	}
	return c.Check(v), nil
}

// Satisfy 是便捷函数:host 满足 constraint 时返回 nil,否则返回错误。
func Satisfy(constraint, host string) error {
	c, err := ParseConstraint(constraint)
	if err != nil {
		return err
	}
	v, err := Parse(host)
	if err != nil {
		return err
	}
	if !c.Check(v) {
		return fmt.Errorf("version: 宿主版本 %s 不满足约束 %q", v, constraint)
	}
	return nil
}
