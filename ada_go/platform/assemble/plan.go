// Package assemble 是 M3 的装配层:把清单变成确定性的装配顺序与报告。
//
// plan 是纯函数:同一批清单永远得到同一个顺序(便于 golden 与评审);
// 真正挂载在 Up 里做,失败会整体回滚,不留下半装状态。
package assemble

import (
	"fmt"
	"sort"
	"strings"

	"github.com/issueye/ada_go_v2/platform/plugin"
)

// 问题类别。报告要能被人直接读懂,所以类别是稳定字符串。
const (
	ProblemDuplicateID       = "duplicate-id"
	ProblemDuplicateProvider = "duplicate-provider"
	ProblemMissingProvider   = "missing-provider"
	ProblemCycle             = "cycle"
)

// CodePlan 是装配计划层的错误码。
const CodePlan = "EASSEMBLEPLAN"

// Problem 是一条装配诊断。
type Problem struct {
	Kind    string `json:"kind"`
	Plugin  string `json:"plugin,omitempty"`
	Service string `json:"service,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

func (p Problem) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]", p.Kind)
	if p.Plugin != "" {
		fmt.Fprintf(&b, " %s", p.Plugin)
	}
	if p.Service != "" {
		fmt.Fprintf(&b, " 需要服务 %q", p.Service)
	}
	if p.Detail != "" {
		fmt.Fprintf(&b, ": %s", p.Detail)
	}
	return b.String()
}

// Error 把报告收敛成一个带码的错误。
type Error struct {
	Code     string
	Problems []Problem
}

func (e *Error) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.String())
	}
	return fmt.Sprintf("%s: %s", e.Code, strings.Join(parts, "; "))
}

// Report 是一次装配计划的产物。
type Report struct {
	Order    []string  // 拓扑顺序;有问题时只包含已确定的部分
	Problems []Problem // 阻断装配的问题
	Warnings []string  // 不阻断,但必须让作者知道
}

// OK 报告计划是否可以执行。
func (r *Report) OK() bool { return len(r.Problems) == 0 }

// Err 把问题收敛成错误;没有问题时返回 nil。
func (r *Report) Err() error {
	if r.OK() {
		return nil
	}
	return &Error{Code: CodePlan, Problems: r.Problems}
}

// Plan 计算装配顺序。
//
// 规则:
//   - 提供者排在消费者之前(拓扑序),同级按 id 排序,保证确定性;
//   - 必需依赖无人提供 → 阻断;提供者本身不可装配 → 级联阻断消费者;
//   - 可选依赖无人提供 → 只告警;
//   - 同一服务被多个启用插件提供 → 阻断(否则「谁生效取决于注册顺序」);
//   - 成环 → 阻断并点名环上的插件。
func Plan(manifests []*plugin.Manifest) *Report {
	report := &Report{}
	byID := make(map[string]*plugin.Manifest, len(manifests))
	enabled := make([]*plugin.Manifest, 0, len(manifests))

	for _, m := range manifests {
		if _, ok := byID[m.ID]; ok {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemDuplicateID, Plugin: m.ID, Detail: "同一个 id 出现了两次",
			})
			continue
		}
		byID[m.ID] = m
		if m.Disabled {
			report.Warnings = append(report.Warnings, fmt.Sprintf("插件 %s 已被 disabled,计划中排除", m.ID))
			continue
		}
		enabled = append(enabled, m)
	}
	sort.Slice(enabled, func(i, j int) bool { return enabled[i].ID < enabled[j].ID })

	providers := map[string][]string{}
	for _, m := range enabled {
		for _, name := range m.Provide {
			providers[name] = append(providers[name], m.ID)
		}
	}
	ambiguous := map[string]bool{}
	for name, ids := range providers {
		if len(ids) > 1 {
			sort.Strings(ids)
			ambiguous[name] = true
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemDuplicateProvider, Service: name,
				Detail: fmt.Sprintf("多个插件都提供该服务: %s", strings.Join(ids, ", ")),
			})
		}
	}

	// 第一轮:直接阻断(缺提供者 / 提供者歧义)。
	reason := map[string]string{}
	for _, m := range enabled {
		for _, name := range m.Inject.Required {
			ids := providers[name]
			switch {
			case len(ids) == 0:
				reason[m.ID] = fmt.Sprintf("缺少必需依赖 %q", name)
				report.Problems = append(report.Problems, Problem{
					Kind: ProblemMissingProvider, Plugin: m.ID, Service: name,
					Detail: "必需依赖没有任何启用的插件提供",
				})
			case ambiguous[name]:
				reason[m.ID] = fmt.Sprintf("依赖 %q 的提供者不唯一", name)
			}
		}
	}

	// 第二轮:级联——提供者本身不可装配时,消费者也不能装配(否则会永久 pending)。
	for changed := true; changed; {
		changed = false
		for _, m := range enabled {
			if _, bad := reason[m.ID]; bad {
				continue
			}
			for _, name := range m.Inject.Required {
				ids := providers[name]
				if len(ids) != 1 {
					continue
				}
				if why, bad := reason[ids[0]]; bad {
					reason[m.ID] = fmt.Sprintf("依赖 %q 的提供者 %s 无法装配", name, ids[0])
					report.Problems = append(report.Problems, Problem{
						Kind: ProblemMissingProvider, Plugin: m.ID, Service: name,
						Detail: fmt.Sprintf("提供者 %s 无法装配(%s)", ids[0], why),
					})
					changed = true
				}
			}
		}
	}

	actionable := make([]*plugin.Manifest, 0, len(enabled))
	actionableSet := map[string]bool{}
	for _, m := range enabled {
		if _, bad := reason[m.ID]; bad {
			continue
		}
		actionable = append(actionable, m)
		actionableSet[m.ID] = true
	}

	// 依赖边:仅在有向装配的插件之间。
	edges := map[string]map[string]bool{}
	addEdge := func(from, to string) {
		if !actionableSet[from] || !actionableSet[to] || from == to {
			return
		}
		if edges[from] == nil {
			edges[from] = map[string]bool{}
		}
		edges[from][to] = true
	}
	for _, m := range actionable {
		for _, name := range m.Inject.Required {
			ids := providers[name]
			if len(ids) == 1 {
				addEdge(ids[0], m.ID)
			}
		}
		for _, name := range m.Inject.Optional {
			ids := providers[name]
			switch {
			case len(ids) == 0:
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("插件 %s 的可选依赖 %q 未提供(按缺失处理,不影响装配)", m.ID, name))
			case ambiguous[name]:
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("插件 %s 的可选依赖 %q 有多个提供者,不建立顺序约束", m.ID, name))
			default:
				addEdge(ids[0], m.ID)
			}
		}
	}

	// Kahn 拓扑排序;ready 集合按 id 排序,保证确定性。
	indeg := make(map[string]int, len(actionable))
	for _, m := range actionable {
		indeg[m.ID] = 0
	}
	for _, tos := range edges {
		for to := range tos {
			indeg[to]++
		}
	}
	var ready []string
	for _, m := range actionable {
		if indeg[m.ID] == 0 {
			ready = append(ready, m.ID)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(actionable))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		next := make([]string, 0, len(edges[id]))
		for to := range edges[id] {
			indeg[to]--
			if indeg[to] == 0 {
				next = append(next, to)
			}
		}
		sort.Strings(next)
		ready = append(ready, next...)
		sort.Strings(ready)
	}
	report.Order = order

	if len(order) < len(actionable) {
		done := map[string]bool{}
		for _, id := range order {
			done[id] = true
		}
		pending := make([]string, 0, len(actionable)-len(order))
		for _, m := range actionable {
			if !done[m.ID] {
				pending = append(pending, m.ID)
			}
		}
		sort.Strings(pending)
		if cycle := findCycle(pending, edges); len(cycle) > 0 {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemCycle, Plugin: strings.Join(cycle, " → "),
				Detail: "依赖成环,无法确定装配顺序",
			})
		} else {
			report.Problems = append(report.Problems, Problem{
				Kind: ProblemCycle, Plugin: strings.Join(pending, ", "),
				Detail: "这些插件无法排序(依赖边不完整)",
			})
		}
	}
	return report
}

// findCycle 在剩余节点里找一条环(DFS + 颜色标记),返回环路径。
func findCycle(nodes []string, edges map[string]map[string]bool) []string {
	inSet := map[string]bool{}
	for _, n := range nodes {
		inSet[n] = true
	}
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var cycle []string
	var dfs func(id string) bool
	dfs = func(id string) bool {
		color[id] = gray
		stack = append(stack, id)
		tos := make([]string, 0, len(edges[id]))
		for to := range edges[id] {
			if inSet[to] {
				tos = append(tos, to)
			}
		}
		sort.Strings(tos)
		for _, to := range tos {
			switch color[to] {
			case white:
				if dfs(to) {
					return true
				}
			case gray:
				start := 0
				for i, x := range stack {
					if x == to {
						start = i
						break
					}
				}
				cycle = append(append([]string{}, stack[start:]...), to)
				return true
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return false
	}
	sorted := append([]string{}, nodes...)
	sort.Strings(sorted)
	for _, n := range sorted {
		if color[n] == white && dfs(n) {
			return cycle
		}
	}
	return nil
}
