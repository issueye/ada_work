package ui

import (
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/egoist/mygo"
	mui "github.com/egoist/mygo/ui"
	"github.com/lobehub/lobe-mygo/pkg/action"
	"github.com/lobehub/lobe-mygo/pkg/display"
	"github.com/lobehub/lobe-mygo/pkg/feedback"
	"github.com/lobehub/lobe-mygo/pkg/field"
	"github.com/lobehub/lobe-mygo/pkg/lucide"
	"github.com/lobehub/lobe-mygo/pkg/nav"
	"github.com/lobehub/lobe-mygo/pkg/overlay"
	"github.com/lobehub/lobe-mygo/pkg/theme"

	"ada_chat/internal/app"
)

// botGlyph is one icon choice; its color tints the avatar and the process
// status, so bots stay distinguishable without a color picker.
type botGlyph struct {
	key, label, color string
	svg               func() *mui.SVG
}

var botGlyphs = []botGlyph{
	{"bot", "机器人", "#7858f9", lucide.Bot},
	{"sparkles", "灵感", "#eb2f96", lucide.Sparkles},
	{"brain", "思考", "#1677ff", lucide.Brain},
	{"code", "代码", "#13c2c2", lucide.Code},
	{"palette", "设计", "#faad14", lucide.Palette},
	{"book", "知识", "#52c41a", lucide.BookOpen},
	{"rocket", "上手", "#f88c13", lucide.Rocket},
	{"wrench", "工具", "#383838", lucide.Wrench},
}

func botGlyphSVG(key string) *mui.SVG {
	for _, g := range botGlyphs {
		if g.key == key {
			return g.svg()
		}
	}
	return lucide.Bot()
}

func botGlyphColor(key string) string {
	for _, g := range botGlyphs {
		if g.key == key {
			return g.color
		}
	}
	return botGlyphs[0].color
}

// compactPath 让侧栏的机器人行只显示最后两级目录；完整路径在提示里。
func compactPath(path string) string {
	path = strings.TrimRight(path, `/\`)
	base := filepath.Base(path)
	parent := filepath.Base(filepath.Dir(path))
	if parent == "" || parent == "." || parent == string(filepath.Separator) {
		return base
	}
	return parent + string(filepath.Separator) + base
}

// botAvatar 用图标的颜色画一枚小方块，侧栏的行和对话框的预览共用它。
func botAvatar(c *mui.Context, icon, color string, size float32) {
	if color == "" {
		color = botGlyphs[0].color
	}
	accent := mui.Hex(color)
	mui.Row(c).Size(size, size).Shrink(0).Radius(size / 4).Center().
		Background(accent.Alpha(0.14)).Children(func() {
		mui.Icon(c, botGlyphSVG(icon)).FontSize(size * 0.55).TextColor(accent)
	})
}

// botSection 列出已经添加的机器人；没有机器人时整段不占位置。
func (u *UI) botSection(c *mui.Context) {
	tk := theme.TokensOf(c)
	bots := u.App.Bots
	if len(bots) == 0 || strings.TrimSpace(u.query) != "" {
		return
	}
	active := 0
	if s := u.App.Current(); s != nil {
		active = s.BotID
	}
	mui.Column(c).Gap(2).Children(func() {
		mui.Row(c).Padding(10, 8, 2, 8).Gap(6).AlignItems(mui.Center).Children(func() {
			display.Text(c, "机器人", display.TextProps{Type: display.TextSecondary, FontSize: 11, Strong: true})
			// mui.Spacer(c)
			// display.Text(c, strconv.Itoa(len(bots)), display.TextProps{Type: display.TextSecondary, FontSize: 11})
		})
		for _, b := range bots {
			u.botRow(c, tk, b, active == b.ID)
		}
	})
}

func (u *UI) botRow(c *mui.Context, tk *theme.Tokens, b *app.Bot, active bool) {
	row := mui.ButtonBase(c).Label(b.Name).Height(44).PaddingX(8).Gap(8).AlignItems(mui.Center).
		Radius(tk.BorderRadiusSM).Cursor(mui.CursorPointer).AlignSelf(mui.Stretch)
	bg := mui.Transparent
	if active {
		bg = tk.FillTertiary
	}
	if row.Hovered() {
		bg = tk.FillSecondary
	}
	row.Background(bg)
	overlay.Tooltip(c, row, botTooltip(b), overlay.TooltipProps{})
	row.Children(func() {
		mui.Box(c).Shrink(0).Children(func() { botAvatar(c, b.Icon, b.Color, 24) })
		mui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() {
			mui.Text(c, b.Name).FontSize(13).FontWeight(500).TextColor(tk.Text).SingleLine()
			path := b.Workspace
			if path == "" {
				path = "未选择工作目录"
			} else {
				path = compactPath(path)
			}
			display.Text(c, path, display.TextProps{Type: display.TextSecondary, FontSize: 11})
		})
		mui.Box(c).Size(7, 7).Shrink(0).Radius(4).Background(botStateColor(tk, b.State))
	})
	u.botContextMenu(c, row, b)
	if row.Clicked() {
		u.App.OpenBotChat(b.ID)
	}
}

// botContextMenu 让机器人的侧栏行可以右键：新建会话、编辑资料，或重启它的进程。
func (u *UI) botContextMenu(c *mui.Context, row mui.Element, b *app.Bot) {
	startLabel := "重启进程"
	if b.State == app.BotStopped || b.State == app.BotError {
		startLabel = "启动进程"
	}
	items := []overlay.MenuItem{
		{Key: "new", Label: "新建会话", Icon: lucide.MessageSquarePlus()},
		{Key: "edit", Label: "编辑信息", Icon: lucide.Pencil()},
		{Key: "restart", Label: startLabel, Icon: lucide.RefreshCw(), Disabled: b.State == app.BotStarting},
	}
	chosen := overlay.ContextMenu(c, row, overlay.DropdownMenuProps{Items: items})
	switch chosen {
	case "new":
		u.newBotSession(c, b)
	case "edit":
		u.openBotEdit(b)
	case "restart":
		u.restartBot(c, b)
	}
}

// newBotSession 新开一条对话并切过去；runtime 侧会为它建独立的 agent session。
func (u *UI) newBotSession(c *mui.Context, b *app.Bot) {
	if _, err := u.App.NewBotSession(b.ID); err != nil {
		feedback.ShowToast(c, feedback.ToastProps{
			Title: "新建会话失败", Description: err.Error(), Type: feedback.ToastError,
		})
		return
	}
	u.App.StartBot(b.ID)
	feedback.ShowToast(c, feedback.ToastProps{
		Title: "已新建会话", Description: b.Name, Type: feedback.ToastSuccess,
	})
	c.Invalidate()
}

func (u *UI) restartBot(c *mui.Context, b *app.Bot) {
	if b.State == app.BotStarting {
		return
	}
	if b.State == app.BotRunning {
		u.App.StopBot(b.ID)
	}
	u.App.StartBot(b.ID)
	feedback.ShowToast(c, feedback.ToastProps{
		Title: "机器人进程已重启", Description: b.Name, Type: feedback.ToastSuccess,
	})
}

func botTooltip(b *app.Bot) string {
	state := botStateLabel(b.State)
	if b.PID > 0 && b.State == app.BotRunning {
		state += " · pid " + strconv.Itoa(b.PID)
	}
	if b.Err != "" && b.State == app.BotError {
		state += " · " + b.Err
	}
	return b.Name + "\n" + b.Workspace + "\n" + state
}

func botStateLabel(s app.BotState) string {
	switch s {
	case app.BotStarting:
		return "启动中"
	case app.BotRunning:
		return "运行中"
	case app.BotError:
		return "启动失败"
	default:
		return "已停止"
	}
}

func botStateColor(tk *theme.Tokens, s app.BotState) mui.Color {
	switch s {
	case app.BotStarting:
		return tk.Warning
	case app.BotRunning:
		return tk.Success
	case app.BotError:
		return tk.Error
	default:
		return tk.TextQuaternary
	}
}

// openBotDialog 每次都从空白开始，只带上最近的工作目录和默认图标。
func (u *UI) openBotDialog() {
	icon := botGlyphs[0].key
	u.botEditID = 0
	u.botDraft = app.Bot{
		Workspace: u.App.Workspace,
		Icon:      icon,
		Color:     botGlyphColor(icon),
	}
	u.botErr = ""
	u.botDirErr = ""
	u.botEnvErr = ""
	u.botEnvRows = nil
	u.botTab = 0
	u.botUp = true
}

// openBotEdit 用现有机器人的资料填满同一个表单；保存走 UpdateBot。
func (u *UI) openBotEdit(b *app.Bot) {
	if b == nil {
		return
	}
	u.botEditID = b.ID
	u.botDraft = app.Bot{
		Name:        b.Name,
		Description: b.Description,
		Instruction: b.Instruction,
		Workspace:   b.Workspace,
		Icon:        b.Icon,
		Color:       b.Color,
	}
	u.botErr = ""
	u.botEnvErr = ""
	u.botEnvRows = envRowsOf(b.Env)
	u.botDirErr = ""
	u.botTab = 0
	u.botUp = true
}

func (u *UI) botDialog(c *mui.Context) {
	tk := theme.TokensOf(c)
	u.botDraft.Color = botGlyphColor(u.botDraft.Icon)
	title, okText := "添加机器人", "创建"
	if u.botEditID != 0 {
		title, okText = "编辑机器人", "保存"
	}
	ok := feedback.Modal(c, &u.botUp, feedback.ModalProps{
		Title: title, Width: 560, OkText: okText, CancelText: "取消",
	}, func() {
		u.botFormBody(c, tk)
	})
	if !ok {
		return
	}
	// 每次提交都从干净的错误状态开始，失败原因只保留到下一次提交。
	u.botErr, u.botDirErr, u.botEnvErr = "", "", ""
	u.botDraft.Name = strings.TrimSpace(u.botDraft.Name)
	if u.botDraft.Name == "" {
		u.botErr = "给它起个名字，侧栏里才认得出来"
		u.botTab = 0
		c.Invalidate()
		return
	}
	env, envErr := rowsToEnv(u.botEnvRows)
	if envErr != "" {
		u.botEnvErr = envErr
		u.botTab = 2
		c.Invalidate()
		return
	}
	u.botDraft.Env = env
	if u.botEditID != 0 {
		bot, err := u.App.UpdateBot(u.botEditID, u.botDraft)
		if err != nil {
			u.botDirErr = err.Error()
			u.botTab = 0
			c.Invalidate()
			return
		}
		u.botUp = false
		u.botEditID = 0
		feedback.ShowToast(c, feedback.ToastProps{
			Title: "机器人信息已更新", Description: bot.Name, Type: feedback.ToastSuccess,
		})
		return
	}
	bot, err := u.App.AddBot(u.botDraft)
	if err != nil {
		u.botDirErr = err.Error()
		u.botTab = 0
		c.Invalidate()
		return
	}
	u.botUp = false
	u.App.OpenBotChat(bot.ID)
	feedback.ShowToast(c, feedback.ToastProps{
		Title: "机器人已添加", Description: bot.Name, Type: feedback.ToastSuccess,
	})
}

// botFormBody 是机器人对话框的主体:预览常驻,其余按三页归类
// (基本 / 提示词 / 环境变量),避免表单越加越长。
func (u *UI) botFormBody(c *mui.Context, tk *theme.Tokens) {
	mui.Column(c).Gap(12).Children(func() {
		u.botPreview(c, tk)
		if u.botEditID != 0 {
			mui.Text(c, "改名与图标立即生效；工作目录或环境变量改动会重启该机器人的进程；系统提示词在下一次进程启动时生效。").
				FontSize(11).TextColor(tk.TextTertiary)
		}
		nav.Tabs(c, &u.botTab, nav.TabsProps{
			Variant: nav.TabsSquare,
			Size:    theme.Small,
			Items: []nav.TabItem{
				{Label: "基本", Icon: lucide.Bot()},
				{Label: "提示词", Icon: lucide.MessageSquare()},
				{Label: "环境变量", Icon: lucide.Terminal()},
			},
		}, func(i int) { u.botPane(c, tk, i) })
	})
}

// botPane 是机器人对话框的一页。校验错误按页归位:保存失败时由 botDialog
// 切到对应页,不把用户丢在一屏之外。
func (u *UI) botPane(c *mui.Context, tk *theme.Tokens, tab int) {
	d := &u.botDraft
	switch tab {
	case 1:
		mui.Column(c).Gap(12).Children(func() {
			botField(c, tk, "系统提示词", false, "每轮对话都会放在最前面，告诉它该怎么做", func() {
				field.TextArea(c, &d.Instruction, field.TextAreaProps{
					Variant: field.InputOutlined, Placeholder: "例如：回答前先读一遍相关代码，再给出具体改动",
					MinRows: 3, MaxRows: 6,
				}).Label("系统提示词")
			})
		})
	case 2:
		mui.Column(c).Gap(12).Children(func() {
			botField(c, tk, "环境变量", false, "只对这个机器人的进程生效；同名变量覆盖全局设置，环境不同会各起一个进程", func() {
				u.botEnvEditor(c, tk)
			})
		})
	default:
		mui.Column(c).Gap(12).Children(func() {
			botField(c, tk, "名称", true, "侧栏和对话里都用这个名字", func() {
				field.Input(c, &d.Name, field.InputProps{
					Variant: field.InputOutlined, Placeholder: "例如：代码助手", Invalid: u.botErr != "",
				}).Label("名称")
			})
			if u.botErr != "" {
				mui.Text(c, u.botErr).FontSize(11).TextColor(tk.Error).Margin(4, 0, 0, 0)
			}
			botField(c, tk, "工作目录", true, "机器人进程在这个目录里启动，也是它的工作区", func() {
				mui.Row(c).Gap(8).AlignItems(mui.Center).Children(func() {
					mui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() {
						field.Input(c, &d.Workspace, field.InputProps{
							Variant: field.InputOutlined, Placeholder: "选择或输入一个目录",
							Invalid: u.botDirErr != "",
						}).Label("工作目录")
					})
					label := "选择目录"
					if u.dirPicking {
						label = "选择中…"
					}
					if action.Button(c, label, action.ButtonProps{Size: theme.Small}).Clicked() && !u.dirPicking {
						u.pickWorkspaceDir()
					}
				})
				if u.botDirErr != "" {
					mui.Text(c, u.botDirErr).FontSize(11).TextColor(tk.Error).Margin(4, 0, 0, 0)
				}
			})
			botField(c, tk, "介绍", false, "一句话说明它擅长什么", func() {
				field.TextArea(c, &d.Description, field.TextAreaProps{
					Variant: field.InputOutlined, Placeholder: "例如：帮我读代码，算清楚每一步",
					MinRows: 2, MaxRows: 3,
				}).Label("介绍")
			})
			botField(c, tk, "图标", false, "挑一个一眼认得出的符号", func() { u.iconPicker(c, tk) })
		})
	}
}

// botField 是对话框里紧凑的一栏：标题、说明在上，控件在下。
func botField(c *mui.Context, tk *theme.Tokens, label string, required bool, desc string, control func()) {
	mui.Column(c).Gap(4).Children(func() {
		mui.Row(c).Gap(4).AlignItems(mui.Center).Children(func() {
			mui.Text(c, label).FontSize(13).FontWeight(600).TextColor(tk.Text)
			if required {
				mui.Text(c, "*").FontSize(13).TextColor(tk.Error)
			}
		})
		if desc != "" {
			mui.Text(c, desc).FontSize(11).TextColor(tk.TextTertiary).SingleLine()
		}
		control()
	})
}

// botPreview 是对话框顶部的即时预览：名字、工作目录和图标还没保存就先画出来。
func (u *UI) botPreview(c *mui.Context, tk *theme.Tokens) {
	d := &u.botDraft
	name := strings.TrimSpace(d.Name)
	if name == "" {
		name = "新机器人"
	}
	where := "选择机器人进程的工作目录"
	if d.Workspace != "" {
		where = "将运行在 " + d.Workspace
	}
	mui.Row(c).Gap(12).AlignItems(mui.Center).Children(func() {
		mui.Box(c).Shrink(0).Children(func() { botAvatar(c, d.Icon, d.Color, 44) })
		mui.Column(c).Gap(2).MinWidth(0).Children(func() {
			mui.Text(c, name).FontSize(14).FontWeight(600).TextColor(tk.Text).SingleLine()
			mui.Text(c, where).FontSize(12).TextColor(tk.TextTertiary).SingleLine()
		})
	})
}

func (u *UI) iconPicker(c *mui.Context, tk *theme.Tokens) {
	f := &u.botDraft
	mui.RadioGroup(c, func() {
		for _, g := range botGlyphs {
			chosen := f.Icon == g.key
			b := mui.RadioBase(c.Key(g.key), &f.Icon, g.key).Size(38, 38).Radius(10).
				Center().Cursor(mui.CursorPointer)
			b.Label(g.label)
			overlay.Tooltip(c, b, g.label, overlay.TooltipProps{})
			bg, fg := tk.FillTertiary, tk.TextSecondary
			if chosen {
				bg, fg = tk.PrimaryBg, mui.Hex(g.color)
			}
			if b.Hovered() {
				bg = tk.FillSecondary
			}
			b.Background(bg).Children(func() {
				mui.Icon(c, g.svg()).FontSize(19).TextColor(fg)
			})
		}
	}).Label("图标").Row().Wrap().Gap(8)
}

// pickWorkspaceDir opens the native folder picker off the UI thread and puts
// the chosen directory into the draft. It is the only way to pick a workspace:
// there is no dropdown, because a workspace is a process working directory.
func (u *UI) pickWorkspaceDir() {
	if u.dirPicking {
		return
	}
	u.dirPicking = true
	pick := u.pickDir
	if pick == nil {
		pick = pickFolder
	}
	apply := func(dir string, err error) {
		u.dirPicking = false
		if err == nil && dir != "" {
			u.botDraft.Workspace = dir
			u.botDirErr = ""
		}
	}
	dispatch := u.dispatch
	if dispatch == nil {
		dir, err := pick()
		apply(dir, err)
		return
	}
	go func() {
		dir, err := pick()
		dispatch(func() { apply(dir, err) })
	}()
}

func pickFolder() (string, error) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Directory: true})
	if err != nil || len(paths) == 0 {
		return "", err
	}
	return paths[0], nil
}

type botEnvRow struct{ Key, Value string }

// envNameRe 与 shell 的变量名一致:字母、数字、下划线,不能以数字开头。
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// envRowsOf 把机器人保存的环境变量摊成可编辑的行(按键名排序,避免每帧跳动)。
func envRowsOf(env map[string]string) []botEnvRow {
	if len(env) == 0 {
		return nil
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]botEnvRow, 0, len(names))
	for _, name := range names {
		rows = append(rows, botEnvRow{Key: name, Value: env[name]})
	}
	return rows
}

// rowsToEnv 校验并合并编辑行:空行忽略、空值等于不设置、重名与非法名报错。
func rowsToEnv(rows []botEnvRow) (map[string]string, string) {
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		name := strings.TrimSpace(row.Key)
		value := row.Value
		if name == "" && strings.TrimSpace(value) == "" {
			continue
		}
		if !envNameRe.MatchString(name) {
			return nil, "环境变量名只能由字母、数字与下划线组成,且不能以数字开头: " + name
		}
		if _, dup := out[name]; dup {
			return nil, "环境变量名重复: " + name
		}
		if strings.TrimSpace(value) == "" {
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, ""
}

// botEnvEditor 是「环境变量」一栏:每行 变量名 + 值 + 删除,底部一个添加按钮。
// 这些值只对这个机器人的进程生效(环境不同会另起进程);它们写进 state.json(0600),
// 与全局供应商设置是两回事:同名变量时以这里为准。
func (u *UI) botEnvEditor(c *mui.Context, tk *theme.Tokens) {
	remove := -1
	mui.Column(c).Gap(6).Children(func() {
		for i := range u.botEnvRows {
			i := i
			mui.Row(c).Gap(6).AlignItems(mui.Center).Children(func() {
				mui.Column(c).Width(170).Children(func() {
					field.Input(c, &u.botEnvRows[i].Key, field.InputProps{
						Variant: field.InputOutlined, Placeholder: "ADA_LLM_API_KEY",
					}).Label("变量名")
				})
				mui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() {
					field.Input(c, &u.botEnvRows[i].Value, field.InputProps{
						Variant: field.InputOutlined, Placeholder: "值（留空等于不设置）",
					}).Label("变量值")
				})
				if action.ActionIcon(c, lucide.Trash2(), small("删除变量")).Clicked() {
					remove = i
				}
			})
		}
		if u.botEnvErr != "" {
			mui.Text(c, u.botEnvErr).FontSize(11).TextColor(tk.Error)
		}
		mui.Row(c).Children(func() {
			if action.Button(c, "添加变量", action.ButtonProps{Size: theme.Small}).Clicked() {
				u.botEnvRows = append(u.botEnvRows, botEnvRow{})
			}
		})
	})
	if remove >= 0 {
		u.botEnvRows = append(u.botEnvRows[:remove], u.botEnvRows[remove+1:]...)
	}
}
