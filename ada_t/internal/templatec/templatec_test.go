package templatec

import (
	"encoding/json"
	"strings"
	"testing"
)

func compileOK(t *testing.T, src string) map[string]any {
	t.Helper()
	plan, warns, err := Compile(src, Options{Component: "C"})
	if err != nil {
		t.Fatalf("编译失败: %v\n模板:\n%s", err, src)
	}
	if len(warns) > 0 {
		t.Errorf("不应有告警: %v", warns)
	}
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestIfElseChain(t *testing.T) {
	plan := compileOK(t, `<Column>
  <Text a-if="a > 1">big</Text>
  <Text a-else-if="a === 1">one</Text>
  <Text a-else>small</Text>
</Column>`)
	// 根的 children[0] 应是 if 节点,3 个分支(else 条件缺省)
	kids := plan["root"].(map[string]any)["children"].([]any)
	if len(kids) != 1 {
		t.Fatalf("if 链应归一为 1 个节点,得到 %d", len(kids))
	}
	ifNode := kids[0].(map[string]any)
	if ifNode["k"] != "if" {
		t.Fatalf("k = %v", ifNode["k"])
	}
	branches := ifNode["branches"].([]any)
	if len(branches) != 3 {
		t.Fatalf("分支数 = %d", len(branches))
	}
	if branches[0].(map[string]any)["cond"] == nil {
		t.Fatal("第一分支应有条件")
	}
	if branches[2].(map[string]any)["cond"] != nil {
		t.Fatal("else 分支应无条件")
	}
}

func TestForWrap(t *testing.T) {
	plan := compileOK(t, `<Column>
  <Row a-for="item, i in state.items" a-key="item"><Text>{{ i }}</Text></Row>
</Column>`)
	kids := plan["root"].(map[string]any)["children"].([]any)
	forNode := kids[0].(map[string]any)
	if forNode["k"] != "for" {
		t.Fatalf("k = %v", forNode["k"])
	}
	if forNode["item"] != "item" || forNode["index"] != "i" {
		t.Fatalf("item/index: %v/%v", forNode["item"], forNode["index"])
	}
	if forNode["src"] == nil || forNode["key"] == nil {
		t.Fatal("src/key 缺失")
	}
	inner := forNode["children"].([]any)[0].(map[string]any)
	if inner["tag"] != "Row" {
		t.Fatalf("子节点 tag = %v", inner["tag"])
	}
}

func TestBareTextAutoWrap(t *testing.T) {
	plan := compileOK(t, `<Column>Hello {{ name }}!</Column>`)
	kids := plan["root"].(map[string]any)["children"].([]any)
	if len(kids) != 1 {
		t.Fatalf("裸文本应包成一个 Text,得到 %d 个子节点", len(kids))
	}
	text := kids[0].(map[string]any)
	if text["tag"] != "Text" {
		t.Fatalf("tag = %v", text["tag"])
	}
	parts := text["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts = %d(Hello/插值/!)", len(parts))
	}
	if parts[0].(map[string]any)["v"] != "Hello " {
		t.Fatalf("静态段(词间空白保留): %q", parts[0].(map[string]any)["v"])
	}
}

func TestInterpolationWhitespaceKept(t *testing.T) {
	plan := compileOK(t, `<Column><Text>{{ $t('echo') }}: {{ state.q }}</Text></Column>`)
	kids := plan["root"].(map[string]any)["children"].([]any)
	text := kids[0].(map[string]any)
	parts := text["parts"].([]any)
	// ": " 是词间空白,必须保留
	if parts[1].(map[string]any)["v"] != ": " {
		t.Fatalf("词间空白应保留: %q", parts[1].(map[string]any)["v"])
	}
}

func TestModel(t *testing.T) {
	plan := compileOK(t, `<Column><TextInput a-model="state.query" /></Column>`)
	kids := plan["root"].(map[string]any)["children"].([]any)
	input := kids[0].(map[string]any)
	model := input["model"].(map[string]any)
	if model["kind"] != "text" {
		t.Fatalf("kind = %v", model["kind"])
	}
	target := model["target"].(map[string]any)
	segs := target["p"].([]any)
	if len(segs) != 2 {
		t.Fatalf("target 路径段: %v", segs)
	}
}

func TestModelOnlyDotPath(t *testing.T) {
	_, _, err := Compile(`<Column><TextInput a-model="items[0]" /></Column>`, Options{})
	if err == nil {
		t.Fatalf("下标路径的 a-model 应拒绝(绑定键要稳定)")
	}
}

func TestSecurityRejections(t *testing.T) {
	cases := map[string]string{
		"on* 属性":   `<Column><Button onclick="x()">B</Button></Column>`,
		"a-html":   `<Column><Text a-html="x">T</Text></Column>`,
		"未知标签":     `<Column><div>x</div></Column>`,
		"未知属性":     `<Column fill-xxx="1"><Text>T</Text></Column>`,
		"on* 大写变体": `<Column><Button onClick="x()">B</Button></Column>`,
	}
	for name, src := range cases {
		_, _, err := Compile(src, Options{})
		if err == nil {
			t.Errorf("%s 应被拒绝", name)
			continue
		}
		if name == "on* 属性" || name == "on* 大写变体" || name == "a-html" {
			if !strings.Contains(err.Error(), "EUISECURITY") {
				t.Errorf("%s 应报 EUISECURITY,得到 %v", name, err)
			}
		}
	}
}

func TestRootConstraints(t *testing.T) {
	// 多根
	_, _, err := Compile(`<Column /><Row />`, Options{})
	if err == nil || !strings.Contains(err.Error(), "根节点") {
		t.Fatalf("多根应拒绝: %v", err)
	}
	// 根带 a-for
	_, _, err = Compile(`<Row a-for="x in y" />`, Options{})
	if err == nil || !strings.Contains(err.Error(), "根元素不能带") {
		t.Fatalf("根 a-for 应拒绝: %v", err)
	}
	// for 与 if 同标签
	_, _, err = Compile(`<Column><Text a-if="a" a-for="x in y">t</Text></Column>`, Options{})
	if err == nil || !strings.Contains(err.Error(), "不能同时") {
		t.Fatalf("for+if 同标签应拒绝: %v", err)
	}
	// else 缺前驱
	_, _, err = Compile(`<Column><Text a-else>x</Text></Column>`, Options{})
	if err == nil || !strings.Contains(err.Error(), "紧跟") {
		t.Fatalf("孤立 a-else 应拒绝: %v", err)
	}
}

func TestWarnMissingKey(t *testing.T) {
	_, warns, err := Compile(`<Column><Row a-for="x in xs"><Text>{{x}}</Text></Row></Column>`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) == 0 {
		t.Fatal("缺 a-key 应有告警")
	}
}

func TestIDsStable(t *testing.T) {
	src := `<Column><Text>{{ a }}</Text><Text>{{ b }}</Text></Column>`
	p1, _, _ := Compile(src, Options{Component: "C"})
	p2, _, _ := Compile(src, Options{Component: "C"})
	b1, _ := json.Marshal(p1)
	b2, _ := json.Marshal(p2)
	if string(b1) != string(b2) {
		t.Fatal("同输入应产生相同 Plan(golden 前提)")
	}
	if !strings.Contains(string(b1), `"id":"n1"`) {
		t.Fatalf("节点应从 n1 编号: %s", b1)
	}
}

func TestCompForAndIf(t *testing.T) {
	// 组件标签支持 a-for / a-if(组件事件不受 native 白名单限制)
	plan := compileOK(t, `<Column>
  <Item a-for="m in state.messages" a-key="m.id" a-prop:msg="m" />
  <Item a-if="state.show" a-prop:msg="state.last" />
</Column>`)
	kids := plan["root"].(map[string]any)["children"].([]any)
	if len(kids) != 2 {
		t.Fatalf("应归一为 for + if 两个节点,得到 %d", len(kids))
	}
	forNode := kids[0].(map[string]any)
	ifNode := kids[1].(map[string]any)
	if forNode["k"] != "for" || ifNode["k"] != "if" {
		t.Fatalf("k = %v / %v", forNode["k"], ifNode["k"])
	}
	inner := forNode["children"].([]any)[0].(map[string]any)
	if inner["k"] != "comp" || inner["name"] != "Item" {
		t.Fatalf("for 内的 comp: %v / %v", inner["k"], inner["name"])
	}
	if inner["props"] == nil {
		t.Fatal("comp 的 a-prop 丢失")
	}
}

func TestEntityDecoding(t *testing.T) {
	cases := map[string]string{
		"&amp;":     "&",
		"&#65;":     "A",
		"&#x41;":    "A",
		"&bogus;":   "&bogus;",
		"a & b":     "a & b",
		"&#xD800;":  "&#xD800;", // 代理区非法
		"&#0;":      "&#0;",
		"&lt;b&gt;": "<b>",
	}
	for in, want := range cases {
		if got := decodeEntities(in); got != want {
			t.Errorf("decodeEntities(%q) = %q, want %q", in, got, want)
		}
	}

	plan := compileOK(t, `<Column>
  <Text>A &amp; B &lt;x&gt; &#65; &#x1F600; &unknown; &nbsp;</Text>
  <TextInput placeholder="Tom &amp; Jerry" />
  <Text>{{ '&amp;' }}</Text>
</Column>`)
	kids := plan["root"].(map[string]any)["children"].([]any)
	text := kids[0].(map[string]any)
	parts := text["parts"].([]any)
	var got string
	for _, p := range parts {
		if v, ok := p.(map[string]any)["v"].(string); ok {
			got += v
		}
	}
	want := "A & B <x> A \U0001F600 &unknown; \u00a0"
	if got != want {
		t.Fatalf("文本实体解码 = %q, want %q", got, want)
	}
	input := kids[1].(map[string]any)
	attrs := input["attrs"].(map[string]any)
	if attrs["placeholder"] != "Tom & Jerry" {
		t.Fatalf("属性实体解码 = %v", attrs["placeholder"])
	}
	// 表达式里的字符串字面量不做实体解码
	exprText := kids[2].(map[string]any)
	exprPart := exprText["parts"].([]any)[0].(map[string]any)
	ast := exprPart["e"].(map[string]any)
	if ast["v"] != "&amp;" {
		t.Fatalf("表达式字面量被实体解码: %v", ast["v"])
	}
}

func TestModelRejectsUnsupportedTag(t *testing.T) {
	_, _, err := Compile(`<Column><Text a-model="state.x">t</Text></Column>`, Options{})
	if err == nil || !strings.Contains(err.Error(), "不支持 a-model") {
		t.Fatalf("非绑定控件的 a-model 应拒绝: %v", err)
	}
}

func TestModelEventConflict(t *testing.T) {
	if _, _, err := Compile(`<Column><TextInput a-model="state.q" a-on:input="log" /></Column>`, Options{}); err == nil || !strings.Contains(err.Error(), "已占用") {
		t.Fatalf("a-model 与 a-on:input 冲突应拒绝: %v", err)
	}
	if _, _, err := Compile(`<Column><TextInput a-model="state.q" a-on:submit="send" /></Column>`, Options{}); err != nil {
		t.Fatalf("a-model + submit 不应冲突: %v", err)
	}
}

func TestModelSplitNumber(t *testing.T) {
	plan := compileOK(t, `<Column><Split a-model="state.size" size="200"><Column/><Column/></Split></Column>`)
	kids := plan["root"].(map[string]any)["children"].([]any)
	split := kids[0].(map[string]any)
	model := split["model"].(map[string]any)
	if model["kind"] != "number" {
		t.Fatalf("Split 的 a-model kind = %v, want number", model["kind"])
	}
}

// TestEventModifiersRejected:修饰符没有 native 语义,不能静默编译进 Plan。
func TestEventModifiersRejected(t *testing.T) {
	_, _, err := Compile(`<Column><Button a-on:click.stop="go">B</Button></Column>`, Options{})
	if err == nil || !strings.Contains(err.Error(), "修饰符") {
		t.Fatalf("事件修饰符应被拒绝: %v", err)
	}
}

func TestDuplicateEventHandler(t *testing.T) {
	_, _, err := Compile(`<Column><Button a-on:click="a" a-on:click="b">x</Button></Column>`, Options{})
	if err == nil || !strings.Contains(err.Error(), "重复的事件") {
		t.Fatalf("重复 a-on 应拒绝: %v", err)
	}
}

func TestSubmitOnlyTextInput(t *testing.T) {
	_, _, err := Compile(`<Column><Button a-on:submit="go">B</Button></Column>`, Options{})
	if err == nil || !strings.Contains(err.Error(), "submit") {
		t.Fatalf("非 TextInput 的 submit 应拒绝: %v", err)
	}
	if _, _, err := Compile(`<Column><TextInput a-on:submit="go" /></Column>`, Options{}); err != nil {
		t.Fatalf("TextInput submit 应通过: %v", err)
	}
}

// TestRejectInvalidHexColor:静态 #hex 在编译期拒绝,避免重放层 panic。
func TestRejectInvalidHexColor(t *testing.T) {
	for _, src := range []string{
		`<Column><Text color="#12">t</Text></Column>`,
		`<Column><Text color="#ggg">t</Text></Column>`,
		`<Column fill background="#12345"><Text>t</Text></Column>`,
	} {
		if _, _, err := Compile(src, Options{}); err == nil {
			t.Errorf("%q 应因非法颜色被拒绝", src)
		}
	}
	if _, _, err := Compile(`<Column><Text color="#abc">t</Text></Column>`, Options{}); err != nil {
		t.Fatalf("合法颜色应通过: %v", err)
	}
	if _, _, err := Compile(`<Column><Text color="gray">t</Text></Column>`, Options{}); err != nil {
		t.Fatalf("命名色应通过: %v", err)
	}
}

// TestComponentTagRejectsDirectives:组件标签不能绕过指令/安全校验。
func TestComponentTagRejectsDirectives(t *testing.T) {
	cases := map[string]string{
		`<Sub a-html="x" />`:     "EUISECURITY",
		`<Sub onclick="x" />`:    "EUISECURITY",
		`<Sub a-ref="x" />`:      "EUISECURITY",
		`<Sub a-class:on="x" />`: "EUITEMPLATE",
	}
	for tag, code := range cases {
		_, _, err := Compile(`<Column>`+tag+`</Column>`, Options{})
		if err == nil {
			t.Errorf("%s 应被拒绝", tag)
			continue
		}
		if !strings.Contains(err.Error(), code) {
			t.Errorf("%s 应报 %s,得到 %v", tag, code, err)
		}
	}
	if _, _, err := Compile(`<Column><Sub foo="1" /></Column>`, Options{}); err != nil {
		t.Fatalf("普通静态 prop 应通过: %v", err)
	}
}

// TestKeyWithoutForRejected:a-key 必须与 a-for 搭配(否则会静默进 Plan 而无人消费)。
func TestKeyWithoutForRejected(t *testing.T) {
	_, _, err := Compile(`<Column><Text a-key="state.k">t</Text></Column>`, Options{})
	if err == nil || !strings.Contains(err.Error(), "a-key 只能") {
		t.Fatalf("孤立 a-key 应拒绝: %v", err)
	}
	if _, _, err := Compile(`<Column><Row a-for="x in xs" a-key="x"><Text>t</Text></Row></Column>`, Options{}); err != nil {
		t.Fatalf("a-for + a-key 应通过: %v", err)
	}
}

// TestMalformedNumberStaysString:畸形数字属性不再被 Sscanf 截断成 float。
func TestMalformedNumberStaysString(t *testing.T) {
	plan := compileOK(t, `<Column gap="1.2.3"><Text>x</Text></Column>`)
	attrs := plan["root"].(map[string]any)["attrs"].(map[string]any)
	if attrs["gap"] != "1.2.3" {
		t.Fatalf("gap = %#v, want 字符串 \"1.2.3\"", attrs["gap"])
	}
}

// TestExprErrorUsesEUIEXPR:表达式错误保留自己的诊断码,而不是混进 EUITEMPLATE。
func TestExprErrorUsesEUIEXPR(t *testing.T) {
	_, _, err := Compile(`<Column><Text a-if="a + ">t</Text></Column>`, Options{})
	if err == nil {
		t.Fatal("非法表达式应报错")
	}
	if !strings.Contains(err.Error(), "EUIEXPR") {
		t.Fatalf("表达式错误应报 EUIEXPR,得到 %v", err)
	}
}
