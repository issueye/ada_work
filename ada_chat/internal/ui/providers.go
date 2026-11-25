package ui

import (
	"net/url"
	"strconv"
	"strings"

	mui "github.com/egoist/mygo/ui"
	"github.com/lobehub/lobe-mygo/pkg/action"
	"github.com/lobehub/lobe-mygo/pkg/display"
	"github.com/lobehub/lobe-mygo/pkg/feedback"
	"github.com/lobehub/lobe-mygo/pkg/field"
	"github.com/lobehub/lobe-mygo/pkg/lucide"
	"github.com/lobehub/lobe-mygo/pkg/nav"
	"github.com/lobehub/lobe-mygo/pkg/theme"

	"ada_chat/internal/app"
)

// defaultThinkingTags 是思考级别的默认标签;用户可以在模型编辑器里移除/新增。
var defaultThinkingTags = []string{"low", "medium", "high"}

// —— 供应商管理:左侧列表 + 右侧[供应商信息][模型列表] ——

func (u *UI) providerManager(c *mui.Context) {
	tk := theme.TokensOf(c)
	mui.Column(c).Gap(8).Children(func() {
		mui.Row(c).Gap(0).MinHeight(430).AlignItems(mui.Stretch).Children(func() {
			u.providerSidebar(c, tk)
			mui.Column(c).Grow(1).Basis(0).Padding(0, 0, 0, 4).Children(func() {
				u.providerDetail(c, tk)
			})
		})
		if u.providerErr != "" {
			mui.Text(c, u.providerErr).FontSize(11).TextColor(tk.Error)
		}
	})
}

func (u *UI) providerSidebar(c *mui.Context, tk *theme.Tokens) {
	mui.Column(c).Width(236).Shrink(0).Padding(0, 12, 0, 0).Gap(6).Children(func() {
		mui.Row(c).Gap(6).AlignItems(mui.Center).Children(func() {
			mui.Text(c, "供应商").FontSize(13).FontWeight(600).TextColor(tk.Text)
			mui.Spacer(c)
			if action.ActionIcon(c, lucide.Plus(), small("添加供应商")).Clicked() {
				u.addProviderDraft()
			}
		})
		mui.Column(c).Gap(2).AlignItems(mui.Stretch).Children(func() {
			for i := range u.providerList {
				i := i
				p := u.providerList[i]
				mui.Row(c).Gap(4).AlignItems(mui.Center).Children(func() {
					name := p.Name
					if name == "" {
						name = "未命名供应商"
					}
					row := mui.ButtonBase(c).Label(name).Row().Gap(6).AlignItems(mui.Center).
						Grow(1).Basis(0).MinWidth(0).Padding(6, 8).Radius(tk.BorderRadiusSM).
						Justify(mui.Start).Cursor(mui.CursorPointer)
					if i == u.providerIdx {
						row.Background(tk.FillSecondary)
					} else if row.Hovered() {
						row.Background(tk.FillTertiary)
					}
					row.Children(func() {
						mui.Column(c).Grow(1).Basis(0).MinWidth(0).AlignItems(mui.Start).Children(func() {
							mui.Text(c, name).FontSize(13).TextColor(tk.Text).SingleLine().TextAlign(mui.Start)
							display.Text(c, providerSubtitle(p), display.TextProps{Type: display.TextSecondary, FontSize: 11}).
								SingleLine().TextAlign(mui.Start)
						})
					})
					if row.Clicked() {
						u.providerIdx = i
						u.providerTab = 0
						u.modelEditor = false
					}
					star := action.ActionIcon(c, lucide.Star(), small("设为当前供应商"))
					if star.Clicked() {
						u.providerActive = p.ID
					}
				})
			}
			if len(u.providerList) == 0 {
				mui.Text(c, "还没有供应商,点右上角 + 添加").FontSize(12).TextColor(tk.TextTertiary)
			}
		})
	})
}

func providerSubtitle(p app.Provider) string {
	kind := p.Kind
	if kind == "" {
		kind = "跟随 preset"
	}
	return kind + " · " + strconv.Itoa(len(p.Models)) + " 个模型"
}

func (u *UI) providerDetail(c *mui.Context, tk *theme.Tokens) {
	if len(u.providerList) == 0 || u.providerIdx < 0 || u.providerIdx >= len(u.providerList) {
		mui.Column(c).Grow(1).Center().Children(func() {
			display.Text(c, "先添加一个供应商", display.TextProps{Type: display.TextSecondary, FontSize: 13})
		})
		return
	}
	nav.Tabs(c, &u.providerTab, nav.TabsProps{
		Variant: nav.TabsSquare,
		Size:    theme.Small,
		Items: []nav.TabItem{
			{Label: "供应商信息", Icon: lucide.Info()},
			{Label: "模型列表", Icon: lucide.Cpu()},
		},
	}, func(tab int) {
		if tab == 1 {
			u.providerModelsPane(c, tk, &u.providerList[u.providerIdx])
		} else {
			u.providerInfoPane(c, tk, &u.providerList[u.providerIdx])
		}
	})
}

func (u *UI) providerInfoPane(c *mui.Context, tk *theme.Tokens, p *app.Provider) {
	mui.Column(c).Gap(10).Padding(2, 2, 0, 0).Children(func() {
		mui.Row(c).Gap(8).AlignItems(mui.Center).Children(func() {
			mui.Text(c, "编辑供应商信息;保存后,新启动的机器人进程才使用它").FontSize(11).TextColor(tk.TextTertiary)
			mui.Spacer(c)
			if action.Button(c, "删除供应商", action.ButtonProps{Size: theme.Small}).Clicked() {
				u.providerDeleteUp = true
			}
		})
		botField(c, tk, "名称", true, "在供应商列表里显示的名字", func() {
			field.Input(c, &p.Name, field.InputProps{
				Variant: field.InputOutlined, Placeholder: "例如：DeepSeek",
			}).Label("名称")
		})
		botField(c, tk, "类型", false, "跟随 preset 时由 agent.preset.json 决定", func() {
			field.Select(c, &p.Kind, field.SelectProps{
				Variant: field.InputOutlined,
				Options: []field.SelectOption{
					{Value: "", Label: "跟随 preset"},
					{Value: "openai", Label: "OpenAI 兼容", Icon: lucide.Cloud()},
					{Value: "replay", Label: "Replay(离线脚本)", Icon: lucide.Play()},
				},
			}).Label("类型")
		})
		if p.Kind == "openai" || p.Kind == "" {
			botField(c, tk, "Endpoint", false, "OpenAI 兼容的 /v1 地址;留空用官方端点", func() {
				field.Input(c, &p.Endpoint, field.InputProps{
					Variant: field.InputOutlined, Placeholder: "https://api.deepseek.com",
				}).Label("Endpoint")
			})
			botField(c, tk, "API Key", false, "保存在本机 config.json;留空时读 ADA_LLM_API_KEY", func() {
				u.apiKeyInput(c, &p.APIKey)
			})
		} else {
			display.Text(c, "Replay 使用 preset 里的离线脚本,不会访问网络。", display.TextProps{
				Type: display.TextSecondary, FontSize: 12,
			})
		}
		mui.Row(c).Gap(8).Children(func() {
			if p.ID == u.providerActive {
				display.Tag(c, "当前供应商", display.TagProps{Variant: display.TagOutlined, Color: display.TagGreen, Size: theme.Small})
			} else if action.Button(c, "设为当前供应商", action.ButtonProps{Size: theme.Small}).Clicked() {
				u.providerActive = p.ID
			}
		})
	})
}

func (u *UI) providerModelsPane(c *mui.Context, tk *theme.Tokens, p *app.Provider) {
	remove := -1
	edit := -1
	mui.Column(c).Gap(8).Padding(2, 2, 0, 0).Children(func() {
		mui.Row(c).Gap(8).AlignItems(mui.Center).Children(func() {
			mui.Text(c, "模型列表:每个模型可以带上下文长度、最大输出、摘要阈值与思考级别").FontSize(11).TextColor(tk.TextTertiary)
			mui.Spacer(c)
			if action.Button(c, "添加模型", action.ButtonProps{Size: theme.Small}).Clicked() {
				u.openModelEditor(-1)
			}
		})
		mui.Column(c).Gap(2).AlignItems(mui.Stretch).Children(func() {
			for i := range p.Models {
				i := i
				m := p.Models[i]
				mui.Row(c).Gap(4).AlignItems(mui.Center).Children(func() {
					row := mui.ButtonBase(c).Label(m.ID).Row().Gap(8).AlignItems(mui.Center).
						Grow(1).Basis(0).MinWidth(0).Padding(6, 8).Radius(tk.BorderRadiusSM).
						Justify(mui.Start).Cursor(mui.CursorPointer)
					if row.Hovered() {
						row.Background(tk.FillTertiary)
					}
					row.Children(func() {
						mui.Column(c).Grow(1).Basis(0).MinWidth(0).AlignItems(mui.Start).Children(func() {
							title := m.ID
							if title == "" {
								title = "未命名模型"
							}
							mui.Text(c, title).FontSize(13).TextColor(tk.Text).SingleLine().TextAlign(mui.Start)
							display.Text(c, modelSummary(m), display.TextProps{Type: display.TextSecondary, FontSize: 11}).
								SingleLine().TextAlign(mui.Start)
						})
					})
					if row.Clicked() {
						p.Model = m.ID
					}
					if action.ActionIcon(c, lucide.Star(), small("设为当前模型")).Clicked() {
						p.Model = m.ID
					}
					if action.ActionIcon(c, lucide.Pencil(), small("编辑模型")).Clicked() {
						edit = i
					}
					if action.ActionIcon(c, lucide.Trash2(), small("删除模型")).Clicked() {
						u.modelDeleteIdx = i
						u.modelDeleteUp = true
					}
				})
			}
			if len(p.Models) == 0 {
				mui.Text(c, "还没有模型,点「添加模型」添加第一个").FontSize(12).TextColor(tk.TextTertiary)
			}
		})
		if u.modelEditor {
			u.modelEditorCard(c, tk, p)
		}
	})
	if remove >= 0 {
		p.Models = append(p.Models[:remove], p.Models[remove+1:]...)
		if p.Model == "" && len(p.Models) > 0 {
			p.Model = p.Models[0].ID
		}
	}
	if edit >= 0 {
		u.openModelEditor(edit)
	}
}

func modelSummary(m app.ModelSpec) string {
	parts := make([]string, 0, 4)
	if m.ContextWindow > 0 {
		parts = append(parts, "上下文 "+strconv.Itoa(m.ContextWindow))
	}
	if m.MaxOutput > 0 {
		parts = append(parts, "最大输出 "+strconv.Itoa(m.MaxOutput))
	}
	if m.SummaryThreshold > 0 {
		parts = append(parts, "摘要阈值 "+strconv.Itoa(m.SummaryThreshold)+"%")
	}
	if m.Thinking != "" {
		parts = append(parts, "思考 "+m.Thinking)
	}
	if len(parts) == 0 {
		return "未设置参数"
	}
	return strings.Join(parts, " · ")
}

func (u *UI) openModelEditor(index int) {
	u.modelEditor = true
	u.modelEdit = index
	u.modelErr = ""
	p := &u.providerList[u.providerIdx]
	m := app.ModelSpec{}
	if index >= 0 && index < len(p.Models) {
		m = p.Models[index]
	}
	u.modelID = m.ID
	u.modelThinking = m.Thinking
	u.modelCtx = intText(m.ContextWindow)
	u.modelMax = intText(m.MaxOutput)
	u.modelSummary = intText(m.SummaryThreshold)
}

func intText(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func (u *UI) modelEditorCard(c *mui.Context, tk *theme.Tokens, p *app.Provider) {
	mui.Column(c).Gap(8).Padding(8).Radius(tk.BorderRadiusSM).Background(tk.FillTertiary).Children(func() {
		label := func(text string) {
			mui.Text(c, text).FontSize(11).TextColor(tk.TextTertiary)
		}
		mui.Row(c).Gap(8).AlignItems(mui.Stretch).Children(func() {
			mui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() {
				label("模型 ID")
				field.Input(c, &u.modelID, field.InputProps{
					Variant: field.InputOutlined, Placeholder: "deepseek-chat",
				}).Label("模型 ID")
			})
			mui.Column(c).Width(118).Children(func() {
				label("上下文长度")
				field.Input(c, &u.modelCtx, field.InputProps{
					Variant: field.InputOutlined, Placeholder: "131072",
				}).Label("上下文长度")
			})
			mui.Column(c).Width(108).Children(func() {
				label("最大输出")
				field.Input(c, &u.modelMax, field.InputProps{
					Variant: field.InputOutlined, Placeholder: "8192",
				}).Label("最大输出")
			})
			mui.Column(c).Width(108).Children(func() {
				label("摘要阈值 %")
				field.Input(c, &u.modelSummary, field.InputProps{
					Variant: field.InputOutlined, Placeholder: "80",
				}).Label("摘要阈值")
			})
		})
		mui.Column(c).Gap(4).Children(func() {
			label("思考级别(点标签选中,× 移除;也可新增自定义标签)")
			before := u.modelThinking
			removeTag := ""
			mui.Row(c).Gap(6).AlignItems(mui.Center).Wrap().Children(func() {
				for _, tag := range u.providerTags {
					tag := tag
					props := display.TagProps{
						Variant:  display.TagOutlined,
						Closable: true,
						OnClose:  func() { removeTag = tag },
					}
					if u.modelThinking == tag {
						props.Variant = display.TagSolid
						props.Color = display.TagPurple
					}
					chip := display.Tag(c, tag, props)
					chip.Cursor(mui.CursorPointer)
					chip.OnClick(func() { u.modelThinking = tag })
				}
			})
			if removeTag != "" {
				// 关闭按钮与标签点击可能落在同一次按下:别把「移除」误当成「选中」。
				if u.modelThinking == removeTag && before != removeTag {
					u.modelThinking = before
				}
				u.removeThinkingTag(removeTag)
			}
			mui.Row(c).Gap(6).AlignItems(mui.Center).Children(func() {
				mui.Column(c).Width(160).Children(func() {
					field.Input(c, &u.newTag, field.InputProps{
						Variant: field.InputOutlined, Placeholder: "新标签,如 minimal",
					}).Label("新增思考级别")
				})
				if action.Button(c, "添加标签", action.ButtonProps{Size: theme.Small}).Clicked() {
					u.addThinkingTag()
				}
			})
		})
		if u.modelErr != "" {
			mui.Text(c, u.modelErr).FontSize(11).TextColor(tk.Error)
		}
		mui.Row(c).Gap(8).Children(func() {
			if action.Button(c, "保存模型", action.ButtonProps{Size: theme.Small}).Clicked() {
				u.commitModel(p)
			}
			if action.Button(c, "取消", action.ButtonProps{Size: theme.Small}).Clicked() {
				u.modelEditor = false
			}
		})
	})
}

func (u *UI) commitModel(p *app.Provider) {
	id := strings.TrimSpace(u.modelID)
	if id == "" {
		u.modelErr = "模型 ID 不能为空"
		return
	}
	for i := range p.Models {
		if p.Models[i].ID == id && i != u.modelEdit {
			u.modelErr = "已有同名模型: " + id
			return
		}
	}
	ctx, err := parseOptional(u.modelCtx)
	if err != "" {
		u.modelErr = "上下文长度" + err
		return
	}
	maxOut, err := parseOptional(u.modelMax)
	if err != "" {
		u.modelErr = "最大输出" + err
		return
	}
	summary, err := parseOptional(u.modelSummary)
	if err != "" {
		u.modelErr = "摘要阈值" + err
		return
	}
	m := app.ModelSpec{
		ID: id, ContextWindow: ctx, MaxOutput: maxOut,
		SummaryThreshold: summary, Thinking: strings.TrimSpace(u.modelThinking),
	}
	if u.modelEdit >= 0 && u.modelEdit < len(p.Models) {
		p.Models[u.modelEdit] = m
	} else {
		p.Models = append(p.Models, m)
	}
	if p.Model == "" {
		p.Model = id
	}
	u.modelEditor = false
	u.modelErr = ""
}

// parseOptional 接受空串(0);负数与非数字给出可读错误。
func parseOptional(s string) (int, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ""
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, "必须是非负整数"
	}
	return n, ""
}

func (u *UI) removeThinkingTag(tag string) {
	tags := make([]string, 0, len(u.providerTags))
	for _, t := range u.providerTags {
		if t != tag {
			tags = append(tags, t)
		}
	}
	u.providerTags = tags
	if u.modelThinking == tag {
		u.modelThinking = ""
	}
}

func (u *UI) addThinkingTag() {
	tag := strings.TrimSpace(u.newTag)
	if tag == "" {
		return
	}
	for _, t := range u.providerTags {
		if t == tag {
			u.newTag = ""
			return
		}
	}
	u.providerTags = append(u.providerTags, tag)
	u.newTag = ""
}

func (u *UI) addProviderDraft() {
	id := "provider-" + strconv.Itoa(len(u.providerList)+1)
	for u.providerHasID(id) {
		id += "-x"
	}
	u.providerList = append(u.providerList, app.Provider{
		ID: id, Name: "供应商 " + strconv.Itoa(len(u.providerList)+1), Kind: "openai",
	})
	u.providerIdx = len(u.providerList) - 1
	u.providerActive = id
	u.providerTab = 0
	u.modelEditor = false
}

func (u *UI) providerHasID(id string) bool {
	for _, p := range u.providerList {
		if p.ID == id {
			return true
		}
	}
	return false
}

func (u *UI) deleteProviderAt(index int) {
	if index < 0 || index >= len(u.providerList) {
		return
	}
	removed := u.providerList[index]
	u.providerList = append(u.providerList[:index], u.providerList[index+1:]...)
	if u.providerIdx >= len(u.providerList) && u.providerIdx > 0 {
		u.providerIdx = len(u.providerList) - 1
	}
	if u.providerActive == removed.ID {
		u.providerActive = ""
		if len(u.providerList) > 0 {
			u.providerActive = u.providerList[0].ID
		}
	}
	u.modelEditor = false
}

func (u *UI) deleteModelAt(index int) {
	if len(u.providerList) == 0 || u.providerIdx < 0 || u.providerIdx >= len(u.providerList) {
		return
	}
	p := &u.providerList[u.providerIdx]
	if index < 0 || index >= len(p.Models) {
		return
	}
	removed := p.Models[index]
	p.Models = append(p.Models[:index], p.Models[index+1:]...)
	if p.Model == removed.ID && len(p.Models) > 0 {
		p.Model = p.Models[0].ID
	}
	if u.modelEdit == index {
		u.modelEditor = false
	}
}

// saveProviders 校验整份供应商列表,写回偏好并同步旧的单供应商字段。
func (u *UI) saveProviders(c *mui.Context) {
	// 模型编辑器还开着就先提交:外层「保存」不应丢掉正在填的参数。
	if u.modelEditor {
		u.commitModel(&u.providerList[u.providerIdx])
		if u.modelEditor {
			u.providerErr = "模型参数未保存: " + u.modelErr
			c.Invalidate()
			return
		}
	}
	if len(u.providerList) == 0 {
		u.providerErr = "至少保留一个供应商"
		c.Invalidate()
		return
	}
	for i := range u.providerList {
		p := &u.providerList[i]
		p.Name = strings.TrimSpace(p.Name)
		p.Endpoint = strings.TrimSpace(p.Endpoint)
		p.APIKey = strings.TrimSpace(p.APIKey)
		if p.Name == "" {
			u.providerErr = "第 " + strconv.Itoa(i+1) + " 个供应商缺少名称"
			c.Invalidate()
			return
		}
		switch p.Kind {
		case "", "openai", "replay":
		default:
			u.providerErr = "供应商 " + p.Name + " 的类型未知"
			c.Invalidate()
			return
		}
		if p.Kind == "openai" && p.Endpoint != "" {
			if parsed, err := url.Parse(p.Endpoint); err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				u.providerErr = "供应商 " + p.Name + " 的 Endpoint 必须是 http(s) 地址"
				c.Invalidate()
				return
			}
		}
		for j := range p.Models {
			p.Models[j].ID = strings.TrimSpace(p.Models[j].ID)
			if p.Models[j].ID == "" {
				u.providerErr = "供应商 " + p.Name + " 有空模型 ID"
				c.Invalidate()
				return
			}
		}
		if p.ID == "" {
			p.ID = "provider-" + strconv.Itoa(i+1)
		}
	}
	if !u.providerHasID(u.providerActive) {
		u.providerActive = u.providerList[0].ID
	}
	u.App.Prefs.Providers = u.providerList
	u.App.Prefs.ActiveProvider = u.providerActive
	u.App.Prefs.ThinkingTags = normalizeTags(u.providerTags)
	u.App.Prefs.SyncLegacy()
	u.providerUp = false
	u.providerErr = ""
	if err := u.App.SavePrefs(); err != nil {
		feedback.ShowToast(c, feedback.ToastProps{Title: "供应商已保存", Description: "但写入配置文件失败: " + err.Error(), Type: feedback.ToastError})
		return
	}
	feedback.ShowToast(c, feedback.ToastProps{Title: "供应商已保存", Type: feedback.ToastSuccess})
}

func normalizeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// cloneProviders 深拷贝模型列表;取消编辑时不影响已保存的偏好。
func cloneProviders(in []app.Provider) []app.Provider {
	out := make([]app.Provider, 0, len(in))
	for _, p := range in {
		cp := p
		cp.Models = append([]app.ModelSpec(nil), p.Models...)
		out = append(out, cp)
	}
	return out
}

// confirmProviderDelete / confirmModelDelete 是删除的二次确认弹窗。
// 只有点「删除」才真的改列表。
func (u *UI) confirmProviderDelete(c *mui.Context) {
	if len(u.providerList) == 0 || u.providerIdx < 0 || u.providerIdx >= len(u.providerList) {
		u.providerDeleteUp = false
		return
	}
	p := u.providerList[u.providerIdx]
	name := p.Name
	if name == "" {
		name = "未命名供应商"
	}
	ok := feedback.Modal(c, &u.providerDeleteUp, feedback.ModalProps{
		Title: "删除供应商", Width: 420, OkText: "删除", CancelText: "取消",
	}, func() {
		display.Text(c, "确定删除「"+name+"」?它的 "+strconv.Itoa(len(p.Models))+" 个模型配置会一起删除。",
			display.TextProps{Type: display.TextSecondary, FontSize: 13})
	})
	if ok {
		u.deleteProviderAt(u.providerIdx)
	}
}

func (u *UI) confirmModelDelete(c *mui.Context) {
	if len(u.providerList) == 0 || u.providerIdx < 0 || u.providerIdx >= len(u.providerList) {
		u.modelDeleteUp = false
		return
	}
	p := &u.providerList[u.providerIdx]
	if u.modelDeleteIdx < 0 || u.modelDeleteIdx >= len(p.Models) {
		u.modelDeleteUp = false
		return
	}
	name := p.Models[u.modelDeleteIdx].ID
	ok := feedback.Modal(c, &u.modelDeleteUp, feedback.ModalProps{
		Title: "删除模型", Width: 400, OkText: "删除", CancelText: "取消",
	}, func() {
		display.Text(c, "确定从「"+p.Name+"」删除模型「"+name+"」?", display.TextProps{Type: display.TextSecondary, FontSize: 13})
	})
	if ok {
		u.deleteModelAt(u.modelDeleteIdx)
		u.modelDeleteIdx = -1
	}
}
