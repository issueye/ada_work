package ui

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/egoist/mygo"
	mui "github.com/egoist/mygo/ui"
	"github.com/lobehub/lobe-mygo/pkg/action"
	"github.com/lobehub/lobe-mygo/pkg/choice"
	"github.com/lobehub/lobe-mygo/pkg/display"
	"github.com/lobehub/lobe-mygo/pkg/feedback"
	"github.com/lobehub/lobe-mygo/pkg/field"
	"github.com/lobehub/lobe-mygo/pkg/form"
	"github.com/lobehub/lobe-mygo/pkg/lucide"
	"github.com/lobehub/lobe-mygo/pkg/theme"

	"github.com/issueye/ada_t/plugin"

	"ada_chat/internal/app"
)

// —— 设置弹窗 ——
//
// Agent、供应商、常规各自是一个独立弹窗:入口在左下角用户的 Popover 与标题栏齿轮。
// 三者共用同一份表单(u.settings);保存时按 scope 只校验本弹窗负责的字段,
// 避免「供应商填错挡住了 Agent 的保存」。

func (u *UI) agentDialog(c *mui.Context) {
	ok := feedback.Modal(c, &u.agentUp, feedback.ModalProps{
		Title: "Agent", Width: 560, OkText: "保存", CancelText: "取消",
	}, func() { u.agentPane(c) })
	if ok {
		u.savePrefs(c, "agent")
	}
}

func (u *UI) providerDialog(c *mui.Context) {
	ok := feedback.Modal(c, &u.providerUp, feedback.ModalProps{
		Title: "供应商", Width: 880, OkText: "保存", CancelText: "取消",
	}, func() {
		u.providerManager(c)
		u.confirmProviderDelete(c)
		u.confirmModelDelete(c)
	})
	if ok {
		u.saveProviders(c)
	}
}

func (u *UI) settingsDialog(c *mui.Context) {
	ok := feedback.Modal(c, &u.settingsUp, feedback.ModalProps{
		Title: "设置", Width: 560, OkText: "保存", CancelText: "取消",
	}, func() {
		u.generalPane(c)
		if u.hasPluginSlot(plugin.SlotSettingsTab) {
			mui.Column(c).PaddingY(8).Children(func() { u.pluginSettingsPane(c) })
		}
	})
	if ok {
		u.savePrefs(c, "general")
	}
}

// openAgentSettings / openProviderSettings 把表单重置成已保存的偏好,再打开对应弹窗。
// 三个入口互斥:同一帧里只允许一个 Modal,否则会叠出两层遮罩。
func (u *UI) openAgentSettings() {
	u.settings.ResetTo(u.App.Prefs)
	u.showKey = false
	u.userMenu = false
	u.settingsUp, u.providerUp = false, false
	u.agentUp = true
}

func (u *UI) openProviderSettings() {
	u.settings.ResetTo(u.App.Prefs)
	u.App.Prefs.EnsureProviders()
	u.showKey = false
	u.userMenu = false
	u.settingsUp, u.agentUp = false, false
	u.providerList = cloneProviders(u.App.Prefs.Providers)
	u.providerTags = append([]string(nil), u.App.Prefs.ThinkingTags...)
	u.newTag = ""
	u.providerDeleteUp, u.modelDeleteUp = false, false
	u.modelDeleteIdx = -1
	u.providerActive = u.App.Prefs.ActiveProvider
	u.providerIdx = 0
	for i := range u.providerList {
		if u.providerList[i].ID == u.providerActive {
			u.providerIdx = i
		}
	}
	u.providerTab = 0
	u.providerErr = ""
	u.modelEditor = false
	u.modelErr = ""
	u.providerUp = true
}

// savePrefs 按 scope 只校验这个弹窗负责的字段,然后写回偏好并落盘。
func (u *UI) savePrefs(c *mui.Context, scope string) {
	v := u.settings.Values
	v.Name = strings.TrimSpace(v.Name)
	v.AgentKind = v.AgentKindValue()
	v.RuntimePath = strings.TrimSpace(v.RuntimePath)
	v.AgentPreset = strings.TrimSpace(v.AgentPreset)
	v.Provider = strings.ToLower(strings.TrimSpace(v.Provider))
	v.Endpoint = strings.TrimSpace(v.Endpoint)
	v.Model = strings.TrimSpace(v.Model)
	v.APIKey = strings.TrimSpace(v.APIKey)
	if scope == "agent" && !u.checkAgent(c, &v) {
		return
	}
	if scope == "provider" && !u.checkProvider(c, &v) {
		return
	}
	// 设置表单是启动时的快照:供应商/标签可能已在别处改过,以 App 上的为准。
	v.Providers = u.App.Prefs.Providers
	v.ActiveProvider = u.App.Prefs.ActiveProvider
	v.ThinkingTags = u.App.Prefs.ThinkingTags
	v.SyncLegacy()
	u.App.Prefs = v
	u.settings.ResetTo(v)
	u.settingsUp, u.agentUp, u.providerUp = false, false, false
	if err := u.App.SavePrefs(); err != nil {
		feedback.ShowToast(c, feedback.ToastProps{
			Title: "设置已保存", Description: "但写入配置文件失败: " + err.Error(), Type: feedback.ToastError,
		})
		return
	}
	feedback.ShowToast(c, feedback.ToastProps{Title: "设置已保存", Type: feedback.ToastSuccess})
}

func (u *UI) checkAgent(c *mui.Context, v *app.Prefs) bool {
	if v.AgentKindValue() == app.AgentACP && v.AgentPreset == "" &&
		strings.TrimSpace(os.Getenv("ADA_BOT_RUNTIME_ARGS")) == "" {
		u.fieldError(c, "preset", "ACP 模式需要 Agent preset 或 ADA_BOT_RUNTIME_ARGS")
		return false
	}
	if v.RuntimePath != "" {
		abs, err := filepath.Abs(v.RuntimePath)
		if err != nil {
			u.fieldError(c, "runtime", "路径非法")
			return false
		}
		if info, err := os.Stat(abs); err != nil || info.IsDir() {
			u.fieldError(c, "runtime", "文件不存在")
			return false
		}
		v.RuntimePath = abs
	}
	if v.AgentPreset != "" {
		abs, err := filepath.Abs(v.AgentPreset)
		if err != nil {
			u.fieldError(c, "preset", "路径非法")
			return false
		}
		if info, err := os.Stat(abs); err != nil || info.IsDir() {
			u.fieldError(c, "preset", "文件不存在")
			return false
		}
		v.AgentPreset = abs
	}
	return true
}

func (u *UI) checkProvider(c *mui.Context, v *app.Prefs) bool {
	switch v.Provider {
	case "", "openai", "replay":
	default:
		u.fieldError(c, "provider", "未知的供应商")
		return false
	}
	if v.Provider == "openai" && v.Endpoint != "" {
		parsed, err := url.Parse(v.Endpoint)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			u.fieldError(c, "endpoint", "必须是 http(s) 地址")
			return false
		}
	}
	return true
}

// fieldError 把错误挂到表单字段上并重画这一帧:弹窗保持打开,错误显示在字段下。
func (u *UI) fieldError(c *mui.Context, name, msg string) {
	u.settings.SetError(name, msg)
	c.Invalidate()
}

func (u *UI) agentPane(c *mui.Context) {
	f := u.settings
	form.View(c, f, form.Props[app.Prefs]{
		Variant: form.Borderless, Layout: form.Vertical, Gap: 8,
	}, func() {
		form.Group(c, f, form.GroupProps{
			Title: "Agent 运行时",
			Desc:  "留空时回落到环境变量 ADA_BOT_RUNTIME / ADA_GO_AGENT_PRESET",
			Fixed: true,
		}, func() {
			form.Field(c, f, form.FieldProps{
				Name: "agentKind", Label: "运行协议",
				Desc:  "RPC 用 ada-rpc-agent；ACP 用 ada acp 或任意 ACP agent",
				Value: f.Values.AgentKind,
			}, func() {
				field.Select(c, &f.Values.AgentKind, field.SelectProps{
					Variant: field.InputOutlined,
					Options: []field.SelectOption{
						{Value: app.AgentRPC, Label: "RPC（ada-rpc-agent）", Icon: lucide.Bot()},
						{Value: app.AgentACP, Label: "ACP（ada acp / 任意 ACP agent）", Icon: lucide.Terminal()},
					},
				}).Label("运行协议")
			})
			runtimeDesc := "ada-rpc-agent 可执行文件；新机器人的工作区进程使用它启动"
			runtimePlaceholder := "选择 ada-rpc-agent.exe"
			presetDesc := "传给 runtime 的 agent.preset.json"
			if f.Values.AgentKindValue() == app.AgentACP {
				runtimeDesc = "ada 可执行文件（或其他 ACP agent）；按 acp --preset 启动"
				runtimePlaceholder = "选择 ada.exe"
				presetDesc = "ACP 会话装配用的 preset；传给 ada acp --preset"
			}
			form.Field(c, f, form.FieldProps{
				Name: "runtime", Label: "Agent 路径",
				Desc:  runtimeDesc,
				Value: f.Values.RuntimePath,
			}, func() {
				u.settingsPath(c, &f.Values.RuntimePath, "runtime", "Agent 路径", runtimePlaceholder)
			})
			form.Field(c, f, form.FieldProps{
				Name: "preset", Label: "Agent preset",
				Desc:  presetDesc,
				Value: f.Values.AgentPreset,
			}, func() {
				u.settingsPath(c, &f.Values.AgentPreset, "preset", "Agent preset", "选择 agent.preset.json")
			})
		})
	})
}

func (u *UI) providerPane(c *mui.Context) {
	f := u.settings
	form.View(c, f, form.Props[app.Prefs]{
		Variant: form.Borderless, Layout: form.Vertical, Gap: 8,
	}, func() {
		form.Group(c, f, form.GroupProps{
			Title: "模型供应商",
			Desc:  "选择新机器人进程使用的 LLM 供应商；已运行的进程不受影响",
			Fixed: true,
		}, func() {
			form.Field(c, f, form.FieldProps{
				Name: "provider", Label: "供应商",
				Desc:  "跟随 preset 时由 agent.preset.json 决定",
				Value: f.Values.Provider,
			}, func() {
				field.Select(c, &f.Values.Provider, field.SelectProps{
					Variant: field.InputOutlined,
					Options: []field.SelectOption{
						{Value: "", Label: "跟随 preset"},
						{Value: "openai", Label: "OpenAI 兼容", Icon: lucide.Cloud()},
						{Value: "replay", Label: "Replay（离线脚本）", Icon: lucide.Play()},
					},
				}).Label("供应商")
			})
			switch f.Values.Provider {
			case "openai":
				form.Field(c, f, form.FieldProps{
					Name: "endpoint", Label: "Endpoint",
					Desc:  "OpenAI 兼容的 /v1 地址；留空用官方端点",
					Value: f.Values.Endpoint,
				}, func() {
					field.Input(c, &f.Values.Endpoint, field.InputProps{
						Variant: field.InputOutlined, Placeholder: "https://api.openai.com/v1",
					}).Label("Endpoint")
				})
				form.Field(c, f, form.FieldProps{
					Name: "model", Label: "模型",
					Desc:  "请求时使用的模型 ID；留空用 gpt-4o-mini",
					Value: f.Values.Model,
				}, func() {
					field.Input(c, &f.Values.Model, field.InputProps{
						Variant: field.InputOutlined, Placeholder: "gpt-4o-mini",
					}).Label("模型")
				})
				form.Field(c, f, form.FieldProps{
					Name: "apiKey", Label: "API Key",
					Desc:  "保存在本机 config.json；留空时读 ADA_LLM_API_KEY",
					Value: f.Values.APIKey,
				}, func() { u.apiKeyInput(c, &f.Values.APIKey) })
			case "replay":
				display.Text(c, "Replay 使用 preset 里的离线脚本，不会访问网络。", display.TextProps{
					Type: display.TextSecondary, FontSize: 12,
				})
			}
		})
	})
}

func (u *UI) generalPane(c *mui.Context) {
	f := u.settings
	form.View(c, f, form.Props[app.Prefs]{
		Variant: form.Borderless, Layout: form.Vertical, Gap: 8,
	}, func() {
		form.Group(c, f, form.GroupProps{Title: "个人资料", Fixed: true}, func() {
			form.Field(c, f, form.FieldProps{
				Name: "name", Label: "显示名称",
				Desc:  "显示在你发送的消息旁",
				Value: f.Values.Name,
			}, func() {
				stretch(c, func() {
					field.Input(c, &f.Values.Name, field.InputProps{
						Variant: field.InputOutlined, Placeholder: "你的名字",
					}).Label("显示名称")
				})
			})
		})
		form.Group(c, f, form.GroupProps{Title: "输入", Fixed: true}, func() {
			form.Group(c, f, form.GroupProps{Title: "外观", Fixed: true}, func() {
				form.Field(c, f, form.FieldProps{
					Name: "theme", Label: "外观",
					Desc:  "跟随系统,或固定浅色/深色",
					Value: f.Values.Theme,
				}, func() {
					field.Select(c, &f.Values.Theme, field.SelectProps{
						Variant: field.InputOutlined,
						Options: []field.SelectOption{
							{Value: "", Label: "跟随系统", Icon: lucide.Monitor()},
							{Value: "light", Label: "浅色", Icon: lucide.Sun()},
							{Value: "dark", Label: "深色", Icon: lucide.Moon()},
						},
					}).Label("外观")
				})
			})
			form.Field(c, f, form.FieldProps{
				Name: "enter", Label: "Enter 发送",
				Desc:  "关闭后 Enter 换行，用 Cmd 或 Ctrl + Enter 发送",
				Value: f.Values.EnterToSend,
			}, func() {
				choiceSwitch(c, &f.Values.EnterToSend, "Enter 发送")
			})
		})
	})
}

// apiKeyInput 默认遮住密钥，并提供临时显示按钮。
func (u *UI) apiKeyInput(c *mui.Context, value *string) {
	mui.Row(c).Gap(4).AlignItems(mui.Center).Children(func() {
		mui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() {
			input := field.Input(c, value, field.InputProps{
				Variant: field.InputOutlined, Placeholder: "sk-…",
			}).Label("API Key")
			if !u.showKey {
				input.Password()
			}
		})
		icon, title := lucide.Eye(), "显示 API Key"
		if u.showKey {
			icon, title = lucide.EyeOff(), "隐藏 API Key"
		}
		if action.ActionIcon(c, icon, small(title)).Clicked() {
			u.showKey = !u.showKey
		}
	})
}

// settingsPath 是设置页的一行路径：输入框 + 系统文件选择器。
func (u *UI) settingsPath(c *mui.Context, value *string, kind, label, placeholder string) {
	mui.Row(c).Gap(8).AlignItems(mui.Center).Children(func() {
		mui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() {
			field.Input(c, value, field.InputProps{
				Variant: field.InputOutlined, Placeholder: placeholder,
			}).Label(label)
		})
		text := "选择文件"
		if u.pathPicking == kind {
			text = "选择中…"
		}
		if action.Button(c, text, action.ButtonProps{Size: theme.Small}).Clicked() && u.pathPicking == "" {
			u.pickSettingsFile(kind, value)
		}
	})
}

// pickSettingsFile opens the native file picker off the UI thread and writes
// the chosen path back into the form.
func (u *UI) pickSettingsFile(kind string, dst *string) {
	if u.pathPicking != "" {
		return
	}
	u.pathPicking = kind
	pick := u.pickFile
	if pick == nil {
		pick = pickAgentFile
	}
	apply := func(path string, err error) {
		u.pathPicking = ""
		if err == nil && path != "" {
			*dst = path
		}
	}
	dispatch := u.dispatch
	if dispatch == nil {
		path, err := pick(kind)
		apply(path, err)
		return
	}
	go func() {
		path, err := pick(kind)
		dispatch(func() { apply(path, err) })
	}()
}

func pickAgentFile(kind string) (string, error) {
	var filters []mygo.FileFilter
	switch kind {
	case "runtime":
		if runtime.GOOS == "windows" {
			filters = []mygo.FileFilter{{Name: "可执行文件", Extensions: []string{"exe"}}}
		}
	case "preset":
		filters = []mygo.FileFilter{{Name: "Agent preset", Extensions: []string{"json"}}}
	}
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Filters: filters})
	if err != nil || len(paths) == 0 {
		return "", err
	}
	return paths[0], nil
}

// stretch 让控件占满表单的右侧栏：field.Input 和 field.Select 返回的是内部元素，
// 不是包住它的那一行。
func stretch(c *mui.Context, control func()) {
	mui.Column(c).AlignSelf(mui.Stretch).AlignItems(mui.Stretch).Children(control)
}

func choiceSwitch(c *mui.Context, on *bool, label string) {
	choice.Switch(c, on, choice.SwitchProps{}).Label(label)
}

func (u *UI) renameDialog(c *mui.Context) {
	tk := theme.TokensOf(c)
	ok := feedback.Modal(c, &u.renameUp, feedback.ModalProps{
		Title: "重命名对话", Width: 440, OkText: "重命名",
	}, func() {
		mui.Column(c).Gap(8).Children(func() {
			mui.Text(c, "取一个在列表里好认的名字。").FontSize(13).TextColor(tk.TextSecondary)
			field.Input(c, &u.renameDraft, field.InputProps{
				Variant: field.InputOutlined, Placeholder: "对话名称",
			}).Label("对话名称")
		})
	})
	if !ok {
		return
	}
	title := strings.TrimSpace(u.renameDraft)
	s := u.App.Current()
	if title == "" || s == nil {
		return
	}
	u.App.Rename(s.ID, title)
	u.renameUp = false
	feedback.ShowToast(c, feedback.ToastProps{Title: "对话已重命名", Description: app.Clip(title, 48), Type: feedback.ToastSuccess})
}

func (u *UI) deleteDialog(c *mui.Context) {
	tk := theme.TokensOf(c)
	s := u.App.SessionByID(u.deleteID)
	if s == nil {
		s = u.App.Current()
	}
	if s == nil {
		u.confirmDel = false
		return
	}
	ok := feedback.Modal(c, &u.confirmDel, feedback.ModalProps{
		Title: "删除对话", Width: 440, OkText: "删除", CancelText: "保留",
	}, func() {
		mui.Column(c).Gap(8).Children(func() {
			mui.Row(c).Gap(8).AlignItems(mui.Center).Children(func() {
				display.Avatar(c, s.Title, display.AvatarProps{Size: 26})
				mui.Text(c, s.Title).FontSize(14).FontWeight(500).TextColor(tk.Text).SingleLine()
			})
			mui.Text(c, "这条对话的消息会从这个窗口移除。撤销按钮会在提示里保留一会儿。").
				FontSize(13).TextColor(tk.TextTertiary)
		})
	})
	if !ok {
		u.deleteID = 0
		return
	}
	at, deleted := u.App.DeleteSession(s.ID)
	u.confirmDel, u.deleteID = false, 0
	if !deleted {
		return
	}
	svc := c.Services()
	feedback.ShowToast(c, feedback.ToastProps{
		Title: "对话已删除", Description: app.Clip(s.Title, 48), Type: feedback.ToastInfo,
		Action: "撤销",
		OnAction: func() {
			u.App.RestoreSession(s, at)
			svc.Invalidate()
		},
	})
}

// approvalDialog 显示 runtime 发来的升级审批请求：允许/拒绝都会把结果回给 runtime，
// 超时未答复时 runtime 自己按拒绝处理（fail-closed）。
func (u *UI) approvalDialog(c *mui.Context) {
	if len(u.App.Approvals) == 0 {
		u.approvalUp = false
		return
	}
	ap := u.App.Approvals[0]
	tk := theme.TokensOf(c)
	u.approvalUp = true
	ok := feedback.Modal(c, &u.approvalUp, feedback.ModalProps{
		Title: "需要审批", Width: 480, OkText: "允许一次", CancelText: "拒绝",
	}, func() {
		botName := "机器人"
		if b := u.App.BotByID(ap.BotID); b != nil {
			botName = b.Name
		}
		mui.Column(c).Gap(6).Children(func() {
			mui.Text(c, ap.Reason).FontSize(14).TextColor(tk.Text)
			detail := botName + " · " + ap.Source
			if ap.Target != "" {
				detail += " · " + ap.Target
			}
			mui.Text(c, detail).FontSize(12).TextColor(tk.TextTertiary)
			if ap.Timeout > 0 {
				mui.Text(c, fmt.Sprintf("超时未答复会按拒绝处理（%s）。", ap.Timeout.Round(time.Second))).
					FontSize(12).TextColor(tk.TextTertiary)
			}
		})
	})
	decide := func(allow bool) {
		reason := "用户允许"
		if !allow {
			reason = "用户拒绝"
		}
		if err := u.App.DecideApproval(ap.ID, allow, reason); err != nil {
			feedback.ShowToast(c, feedback.ToastProps{
				Title: "审批未送达", Description: err.Error(), Type: feedback.ToastError,
			})
		}
		u.approvalUp = false
		c.Invalidate()
	}
	if ok {
		decide(true)
		return
	}
	// 取消/关闭按钮会把开关置回 false；仍为 true 表示面板还开着。
	if !u.approvalUp {
		decide(false)
	}
}
