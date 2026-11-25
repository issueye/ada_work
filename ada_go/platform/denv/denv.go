// Package denv 读取 .env 文件,并把其中的变量补进**进程环境**。
//
// 定位:这是进程入口(宿主适配器)的配置引导,不是插件能力——插件读环境变量
// 仍然走既有路径(`secrets-env` 读 `ADA_SECRET_*`、`llm-openai` 读它的 apiKeyEnv),
// 本包只负责在启动时把本机文件里的值放进去。
//
// 规则(每条都对应一个可验证的边界):
//   - **真实环境优先**:文件只补进程里还没有的键,CI/命令行临时覆盖不会被文件压掉;
//   - 多个文件按给定顺序叠加:后一个文件可以覆盖前面**文件**设的值,但压不过真实环境;
//   - 语法是 dotenv 的常用子集:KEY=VALUE、`export KEY=VALUE`、整行 # 注释、
//     单/双引号、双引号里的 \n \r \t \" \\ 转义、无引号值的行内注释(` #...`);
//   - 键名必须是 `[A-Za-z_][A-Za-z0-9_]*`;非法行**直接报错并给行号**,
//     不静默跳过——一个拼错的键名被忽略,比启动失败更难排查;
//   - 不打印值,也不返回值的日志;`Apply` 只返回实际设置的键数。
package denv

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// keyRe 限制键名:与 shell 的变量名一致,避免把奇怪的东西塞进环境。
var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadFile 读取并解析一个 .env 文件。文件不存在返回 (nil, nil):调用方可以
// 把它当成「没配」而不是错误。
func LoadFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("denv: 读取 %s 失败: %w", path, err)
	}
	vars, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("denv: %s: %w", path, err)
	}
	return vars, nil
}

// Parse 解析 .env 文本;行号从 1 开始,错误信息里带上它。
func Parse(text string) (map[string]string, error) {
	// Windows 记事本会带 BOM;它不是内容的一部分。
	text = strings.TrimPrefix(text, "\ufeff")
	out := make(map[string]string)
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("第 %d 行缺少 '='", i+1)
		}
		key := strings.TrimSpace(line[:eq])
		if !keyRe.MatchString(key) {
			return nil, fmt.Errorf("第 %d 行的键名非法: %q(只允许字母数字与下划线,且不能以数字开头)", i+1, key)
		}
		value, err := parseValue(strings.TrimLeft(line[eq+1:], " \t"))
		if err != nil {
			return nil, fmt.Errorf("第 %d 行: %w", i+1, err)
		}
		out[key] = value
	}
	return out, nil
}

// parseValue 处理三种形态:单引号(原样)、双引号(支持转义)、无引号(去行内注释)。
func parseValue(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	switch s[0] {
	case '\'':
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", fmt.Errorf("单引号没有闭合")
		}
		rest := strings.TrimSpace(s[end+2:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", fmt.Errorf("单引号值后面有多余内容: %q", rest)
		}
		return s[1 : end+1], nil
	case '"':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				case '"':
					b.WriteByte('"')
				case '\\':
					b.WriteByte('\\')
				default:
					// 未知转义保留字符本身,与多数 dotenv 实现一致。
					b.WriteByte(s[i])
				}
				continue
			}
			if c == '"' {
				rest := strings.TrimSpace(s[i+1:])
				if rest != "" && !strings.HasPrefix(rest, "#") {
					return "", fmt.Errorf("双引号值后面有多余内容: %q", rest)
				}
				return b.String(), nil
			}
			b.WriteByte(c)
		}
		return "", fmt.Errorf("双引号没有闭合")
	default:
		if at := inlineCommentAt(s); at >= 0 {
			s = s[:at]
		}
		return strings.TrimSpace(s), nil
	}
}

// inlineCommentAt 找无引号值里的行内注释:`#` 只有在前面是空白时才当注释,
// 这样 URL 片段里的 # 不会被误切。
func inlineCommentAt(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return i - 1
		}
	}
	return -1
}

// Apply 按顺序加载若干 .env 文件并写入进程环境。
//
// 返回实际设置的键数(已存在的真实环境变量不算)。任一文件解析失败即停止,
// 已设置的部分保留——调用方据此决定是「退出」还是「继续用剩下的配置」。
func Apply(paths ...string) (int, error) {
	own := map[string]bool{}
	applied := 0
	for _, path := range paths {
		vars, err := LoadFile(path)
		if err != nil {
			return applied, err
		}
		for key, value := range vars {
			if _, ok := os.LookupEnv(key); ok && !own[key] {
				continue // 真实环境优先
			}
			if err := os.Setenv(key, value); err != nil {
				return applied, fmt.Errorf("denv: 设置 %s 失败: %w", key, err)
			}
			if !own[key] {
				applied++
			}
			own[key] = true
		}
	}
	return applied, nil
}
