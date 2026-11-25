// Package nodemods 是宿主自建的 Node 内置模块源码。
//
// 这里放两类模块:纯 JS 的(path / os / events / util / assert / url / querystring /
// string_decoder),以及靠 __adaHost 注入能力做 I/O 的(process / child_process /
// http / https / dns / net / crypto)。node:fs / node:fs/promises / node:buffer 由
// moejs 的 node 包提供,不在这个目录里。
package nodemods

import (
	"embed"
	"sort"
	"strings"
)

//go:embed src/*.js
var sources embed.FS

// files 是「模块名 → 源码文件」。node: 前缀与裸名是同一份实现。
var files = map[string]string{
	"path":              "src/path.js",
	"os":                "src/os.js",
	"events":            "src/events.js",
	"util":              "src/util.js",
	"assert":            "src/assert.js",
	"url":               "src/url.js",
	"querystring":       "src/querystring.js",
	"string_decoder":    "src/string_decoder.js",
	"process":           "src/process.js",
	"child_process":     "src/child_process.js",
	"http":              "src/http.js",
	"https":             "src/https.js",
	"dns":               "src/dns.js",
	"dns/promises":      "src/dns_promises.js",
	"net":               "src/net.js",
	"crypto":            "src/crypto.js",
	"zlib":              "src/zlib.js",
	"timers":            "src/timers.js",
	"timers/promises":   "src/timers_promises.js",
	"stream":            "src/stream.js",
	"stream/promises":   "src/stream_promises.js",
	"readline":          "src/readline.js",
	"readline/promises": "src/readline_promises.js",
	"tty":               "src/tty.js",
	"perf_hooks":        "src/perf_hooks.js",
}

// Modules 返回已实现的模块名(排序后)。
func Modules() []string {
	out := make([]string, 0, len(files))
	for name := range files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Source 返回模块的 ESM 源码;第二个返回值为 false 表示没有实现。
func Source(name string) (string, bool) {
	name = strings.TrimPrefix(name, "node:")
	file, ok := files[name]
	if !ok {
		return "", false
	}
	src, err := sources.ReadFile(file)
	if err != nil {
		return "", false
	}
	return string(src), true
}
