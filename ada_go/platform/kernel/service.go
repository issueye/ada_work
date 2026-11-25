package kernel

import (
	"sync"
	"sync/atomic"
)

// ServiceSpec 描述一个服务的契约：名字、主版本、形态。
// 版本用于依赖门控：消费者声明最低版本，提供者版本不够就视为依赖未满足。
type ServiceSpec struct {
	Name    string
	Version int
	Kind    string // 目前只有 "value"；为后续 stream/router 预留
}

// label 是隔离槽的身份。同名字符串复用同一指针，
// 因此「同一标签 = 同一槽」，不需要额外的字符串比较规则。
type label struct{ name string }

func (l *label) String() string {
	if l == nil {
		return ""
	}
	return l.name
}

type svcKey struct {
	name string
	lab  *label
}

// svcImpl 是某个服务槽里的一份实现。UID 单调递增，
// 任何「提供者换了实现」都能被消费者通过 epoch 察觉。
type svcImpl struct {
	uid        uint64
	spec       ServiceSpec
	value      any
	provider   *Fiber
	dependents map[string]*Fiber
}

// serviceTable 是全部服务槽。读多写少，用 RWMutex。
type serviceTable struct {
	mu   sync.RWMutex
	impl map[svcKey]*svcImpl
	uid  atomic.Uint64
}

func newServiceTable() *serviceTable {
	return &serviceTable{impl: make(map[svcKey]*svcImpl)}
}

// provide 注册实现。同一槽重复注册是错误：服务值是接口契约，
// 两个插件同时写同一个名字会造成「谁生效取决装配顺序」的随机行为。
func (t *serviceTable) provide(f *Fiber, lab *label, spec ServiceSpec, v any) (*svcImpl, error) {
	key := svcKey{name: spec.Name, lab: lab}
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur, ok := t.impl[key]; ok {
		return nil, Fail(CodeServiceRegistered, "服务 %q 已由插件 %s 提供", spec.Name, cur.provider.id)
	}
	if spec.Version <= 0 {
		spec.Version = 1
	}
	im := &svcImpl{
		uid:        t.uid.Add(1),
		spec:       spec,
		value:      v,
		provider:   f,
		dependents: make(map[string]*Fiber),
	}
	t.impl[key] = im
	return im, nil
}

// release 注销实现；返回仍依赖它的插件数量，供调用方决定是否告警。
func (t *serviceTable) release(im *svcImpl) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, cur := range t.impl {
		if cur == im {
			delete(t.impl, k)
			break
		}
	}
	return len(im.dependents)
}

// set 只允许提供者自己更新值（单写者）。
func (t *serviceTable) set(im *svcImpl, f *Fiber, v any) error {
	if im.provider != f {
		return Fail(CodeNotProvider, "服务 %q 只能由提供者更新", im.spec.Name)
	}
	t.mu.Lock()
	im.value = v
	t.mu.Unlock()
	return nil
}

// resolve 按「名字 + 隔离槽」找实现。隔离槽没提供时**不**回落到全局槽：
// 回落到全局会让隔离失效，而且这种失效很难在运行期察觉。
func (t *serviceTable) resolve(name string, lab *label) *svcImpl {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.impl[svcKey{name: name, lab: lab}]
}

func (t *serviceTable) depend(im *svcImpl, f *Fiber) {
	t.mu.Lock()
	im.dependents[f.id] = f
	t.mu.Unlock()
}

func (t *serviceTable) undepend(im *svcImpl, f *Fiber) {
	t.mu.Lock()
	delete(im.dependents, f.id)
	t.mu.Unlock()
}

func (t *serviceTable) dependentsOf(im *svcImpl) []*Fiber {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*Fiber, 0, len(im.dependents))
	for _, f := range im.dependents {
		out = append(out, f)
	}
	return out
}

func (t *serviceTable) snapshot() []ServiceInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]ServiceInfo, 0, len(t.impl))
	for _, im := range t.impl {
		deps := make([]string, 0, len(im.dependents))
		for id := range im.dependents {
			deps = append(deps, id)
		}
		out = append(out, ServiceInfo{
			Name:       im.spec.Name,
			Version:    im.spec.Version,
			Kind:       im.spec.Kind,
			Provider:   im.provider.id,
			UID:        im.uid,
			Dependents: deps,
		})
	}
	return out
}
