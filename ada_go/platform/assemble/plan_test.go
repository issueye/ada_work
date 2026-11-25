package assemble_test

import (
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/assemble"
	"github.com/issueye/ada_go_v2/platform/plugin"
)

func mf(id string, provide, required, optional []string) *plugin.Manifest {
	return &plugin.Manifest{
		ID: id, Version: "0.1.0", Kind: "go",
		Provide: provide,
		Inject:  plugin.Inject{Required: required, Optional: optional},
	}
}

// 1. 拓扑顺序确定:提供者在前,同级按 id 排序;可选依赖缺失只告警。
func TestPlanOrderIsDeterministic(t *testing.T) {
	ms := []*plugin.Manifest{
		mf("presenter", nil, []string{"upper"}, []string{"telemetry"}),
		mf("upper", []string{"upper"}, []string{"greeting"}, nil),
		mf("greeter", []string{"greeting"}, nil, nil),
	}
	rep := assemble.Plan(ms)
	if !rep.OK() {
		t.Fatalf("计划应通过: %v", rep.Err())
	}
	if got := strings.Join(rep.Order, ","); got != "greeter,upper,presenter" {
		t.Fatalf("顺序不正确: %s", got)
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "telemetry") {
		t.Fatalf("可选依赖缺失应告警: %v", rep.Warnings)
	}
}

// 2. 必需依赖无人提供 → 阻断,且该插件不进入顺序。
func TestPlanRejectsMissingProvider(t *testing.T) {
	rep := assemble.Plan([]*plugin.Manifest{mf("needs-x", nil, []string{"x"}, nil)})
	if rep.OK() {
		t.Fatalf("缺提供者应失败")
	}
	if len(rep.Order) != 0 {
		t.Fatalf("缺依赖的插件不应进入顺序: %v", rep.Order)
	}
	if p := rep.Problems[0]; p.Kind != assemble.ProblemMissingProvider || p.Plugin != "needs-x" || p.Service != "x" {
		t.Fatalf("问题不正确: %+v", p)
	}
}

// 3. 同一服务多个提供者 → 阻断;依赖它的消费者不能被排进顺序。
func TestPlanRejectsDuplicateProvider(t *testing.T) {
	rep := assemble.Plan([]*plugin.Manifest{
		mf("a", []string{"svc"}, nil, nil),
		mf("b", []string{"svc"}, nil, nil),
		mf("c", nil, []string{"svc"}, nil),
	})
	if rep.OK() {
		t.Fatalf("重复提供者应失败")
	}
	for _, id := range rep.Order {
		if id == "c" {
			t.Fatalf("依赖歧义的消费者不应进入顺序: %v", rep.Order)
		}
	}
}

// 4. 成环 → 阻断,并点名环上的插件。
func TestPlanRejectsCycle(t *testing.T) {
	rep := assemble.Plan([]*plugin.Manifest{
		mf("a", []string{"x"}, []string{"y"}, nil),
		mf("b", []string{"y"}, []string{"x"}, nil),
	})
	if rep.OK() {
		t.Fatalf("成环应失败")
	}
	found := false
	for _, p := range rep.Problems {
		if p.Kind == assemble.ProblemCycle {
			found = true
			if !strings.Contains(p.Plugin, "→") {
				t.Fatalf("环应点名路径: %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("应报环: %+v", rep.Problems)
	}
}

// 5. 级联阻断:提供者自己装不起来,消费者也不能装(否则会永久 pending)。
func TestPlanCascadesBlockedProvider(t *testing.T) {
	rep := assemble.Plan([]*plugin.Manifest{
		mf("provider", []string{"svc"}, []string{"missing"}, nil),
		mf("consumer", nil, []string{"svc"}, nil),
	})
	if rep.OK() {
		t.Fatalf("级联阻断应失败")
	}
	if len(rep.Order) != 0 {
		t.Fatalf("被阻断的插件都不应进入顺序: %v", rep.Order)
	}
	if len(rep.Problems) < 2 {
		t.Fatalf("应同时报告缺失与级联: %+v", rep.Problems)
	}
}

// 6. disabled 插件被排除并告警;重复 id 被阻断。
func TestPlanDisabledAndDuplicateID(t *testing.T) {
	off := mf("off", []string{"x"}, nil, nil)
	off.Disabled = true
	rep := assemble.Plan([]*plugin.Manifest{mf("a", nil, nil, nil), off})
	if !rep.OK() {
		t.Fatalf("只有 disabled 时不应阻断: %v", rep.Err())
	}
	if len(rep.Order) != 1 || rep.Order[0] != "a" {
		t.Fatalf("disabled 插件不应进入顺序: %v", rep.Order)
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "disabled") {
		t.Fatalf("应告警 disabled: %v", rep.Warnings)
	}

	dup := assemble.Plan([]*plugin.Manifest{mf("a", nil, nil, nil), mf("a", nil, nil, nil)})
	if dup.OK() {
		t.Fatalf("重复 id 应失败")
	}
}
