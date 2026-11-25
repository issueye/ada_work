package install_test

import (
	"archive/zip"
	"bytes"
	"context"

	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/install"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_jsrt/compile"
)

const tsPluginSource = `type Ctx = { log: (m: string) => void }
export function apply(ctx: Ctx): void { ctx.log("hi") }`

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

// writeTSPlugin 写一个可编译的 ts 插件,返回清单原文与 Artifact 摘要。
func writeTSPlugin(t *testing.T) (dir string, raw []byte, digest string) {
	t.Helper()
	dir = t.TempDir()
	manifest := `{"id":"ts-demo","version":"0.1.0","kind":"ts","entry":"plugin.ts","provide":["ts.demo"]}`
	writeFile(t, dir, "plugin.ts", tsPluginSource)
	writeFile(t, dir, "plugin.json", manifest)
	raw = []byte(manifest)
	sources, err := loader.CollectSources(dir)
	if err != nil {
		t.Fatalf("收集源码失败: %v", err)
	}
	artifact, err := compile.Build("ts-demo", "plugin.ts", sources, compile.Options{})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	return dir, raw, artifact.Digest
}

func writeSignature(t *testing.T, dir, keyID string, sig []byte) {
	t.Helper()
	body := install.Signature{KeyID: keyID, Algo: "ed25519", Signature: base64.StdEncoding.EncodeToString(sig)}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化签名失败: %v", err)
	}
	writeFile(t, dir, install.SignatureFile, string(data))
}

func newKeyRing(t *testing.T, trust caps.Trust) (map[string]install.Key, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	return map[string]install.Key{"k1": {Public: pub, Trust: trust}}, priv
}

// 1. 合法签名:验签通过,信任来自密钥。
func TestSignedVerified(t *testing.T) {
	dir, raw, digest := writeTSPlugin(t)
	keys, priv := newKeyRing(t, caps.TrustVerified)
	writeSignature(t, dir, "k1", install.Sign(priv, raw, digest))

	info, err := install.VerifyDir(dir, install.Options{Keys: keys})
	if err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	if !info.Signed || info.Trust != caps.TrustVerified {
		t.Fatalf("信任信息不正确: %+v", info)
	}
	if info.Digest != digest {
		t.Fatalf("摘要不一致: %s != %s", info.Digest, digest)
	}
}

// 2. 清单被篡改 → 验签失败。
func TestTamperedManifest(t *testing.T) {
	dir, raw, digest := writeTSPlugin(t)
	keys, priv := newKeyRing(t, caps.TrustVerified)
	writeSignature(t, dir, "k1", install.Sign(priv, raw, digest))
	writeFile(t, dir, "plugin.json", string(raw)+"  ")

	if _, err := install.VerifyDir(dir, install.Options{Keys: keys}); err == nil || !strings.Contains(err.Error(), "验签") {
		t.Fatalf("篡改清单应验签失败,实际 %v", err)
	}
}

// 3. 未知密钥 / 错误签名 → 拒绝。
func TestUnknownKeyAndBadSignature(t *testing.T) {
	dir, raw, digest := writeTSPlugin(t)
	keys, _ := newKeyRing(t, caps.TrustVerified)
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	_ = otherPub
	writeSignature(t, dir, "k1", install.Sign(otherPriv, raw, digest))
	if _, err := install.VerifyDir(dir, install.Options{Keys: keys}); err == nil {
		t.Fatalf("错误密钥签名应失败")
	}

	writeSignature(t, dir, "nope", make([]byte, ed25519.SignatureSize))
	if _, err := install.VerifyDir(dir, install.Options{Keys: keys}); err == nil || !strings.Contains(err.Error(), "未知签名密钥") {
		t.Fatalf("未知密钥应报错,实际 %v", err)
	}
}

// 4. 未签名:默认按 third-party;要求签名时拒绝。
func TestUnsigned(t *testing.T) {
	dir, _, _ := writeTSPlugin(t)
	info, err := install.VerifyDir(dir, install.Options{})
	if err != nil {
		t.Fatalf("未签名(不强制)应通过: %v", err)
	}
	if info.Signed || info.Trust != caps.TrustThirdParty {
		t.Fatalf("未签名信任不正确: %+v", info)
	}
	if _, err := install.VerifyDir(dir, install.Options{RequireSignature: true}); err == nil {
		t.Fatalf("要求签名时应拒绝")
	}
}

// 5. Go 插件:信任 builtin,不参与 Artifact 签名。
func TestGoPluginBuiltin(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "plugin.json", `{"id":"go-demo","version":"0.1.0","kind":"go"}`)
	info, err := install.VerifyDir(dir, install.Options{RequireSignature: true})
	if err != nil {
		t.Fatalf("Go 插件应通过(签名只针对 Artifact): %v", err)
	}
	if info.Trust != caps.TrustBuiltin || info.Signed {
		t.Fatalf("Go 插件信任不正确: %+v", info)
	}
}

// 6. 安装:原子落地,拒绝覆盖既有版本。
func TestInstallAtomic(t *testing.T) {
	dir, raw, digest := writeTSPlugin(t)
	keys, priv := newKeyRing(t, caps.TrustVerified)
	writeSignature(t, dir, "k1", install.Sign(priv, raw, digest))

	dest := t.TempDir()
	info, err := install.Install(dir, dest, install.Options{Keys: keys})
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	want := filepath.Join(dest, "ts-demo@0.1.0")
	if info.Dir != want {
		t.Fatalf("安装目录不正确: %s", info.Dir)
	}
	if _, err := os.Stat(filepath.Join(want, "plugin.ts")); err != nil {
		t.Fatalf("插件文件未复制: %v", err)
	}
	if _, err := install.Install(dir, dest, install.Options{Keys: keys}); err == nil || !strings.Contains(err.Error(), "拒绝覆盖") {
		t.Fatalf("重复安装应拒绝覆盖,实际 %v", err)
	}
}

// 7. 目录里的符号链接拒绝安装。
func TestRejectsSymlink(t *testing.T) {
	dir, raw, digest := writeTSPlugin(t)
	keys, priv := newKeyRing(t, caps.TrustVerified)
	writeSignature(t, dir, "k1", install.Sign(priv, raw, digest))

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写外部文件失败: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "x.txt"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("平台无法创建符号链接: %v", err)
	}
	if _, err := install.Install(dir, t.TempDir(), install.Options{Keys: keys}); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("符号链接应拒绝,实际 %v", err)
	}
}

// 8. 安装上限:文件数与总字节数。
func TestInstallLimits(t *testing.T) {
	dir, raw, digest := writeTSPlugin(t)
	keys, priv := newKeyRing(t, caps.TrustVerified)
	writeSignature(t, dir, "k1", install.Sign(priv, raw, digest))
	writeFile(t, dir, "extra.txt", "x")

	if _, err := install.Install(dir, t.TempDir(), install.Options{Keys: keys, MaxFiles: 1}); err == nil || !strings.Contains(err.Error(), "文件数") {
		t.Fatalf("文件数超限应拒绝,实际 %v", err)
	}
	if _, err := install.Install(dir, t.TempDir(), install.Options{Keys: keys, MaxBytes: 8}); err == nil || !strings.Contains(err.Error(), "总大小") {
		t.Fatalf("总大小超限应拒绝,实际 %v", err)
	}
}

// 9. 安装后写索引;List 能读回。
func TestIndexWrittenAndListed(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "plugin.json"),
		[]byte(`{"id":"idx-demo","version":"0.1.0","kind":"go","caps":["fs.read"]}`), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	dest := t.TempDir()
	if _, err := install.Install(src, dest, install.Options{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	entries, err := install.List(dest)
	if err != nil {
		t.Fatalf("读取索引失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("索引应有 1 条,实际 %d", len(entries))
	}
	e := entries[0]
	if e.ID != "idx-demo" || e.Trust != string(caps.TrustBuiltin) || e.Signed {
		t.Fatalf("索引条目不正确: %+v", e)
	}
	if len(e.Caps) != 1 || e.Caps[0] != "fs.read" {
		t.Fatalf("索引未记录能力: %+v", e)
	}
}

// 10. 空目录:List 返回空,不报错。
func TestListEmpty(t *testing.T) {
	entries, err := install.List(t.TempDir())
	if err != nil {
		t.Fatalf("空目录不应报错: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("空目录应无条目,实际 %d", len(entries))
	}
}

// 11. 索引损坏:明确报错。
func TestListCorrupt(t *testing.T) {
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, install.IndexFile), []byte("not json"), 0o644); err != nil {
		t.Fatalf("写坏索引失败: %v", err)
	}
	if _, err := install.List(dest); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Fatalf("损坏索引应报错,实际 %v", err)
	}
}

// 12. 卸载:删除该 id 的全部版本并清空索引条目。
func TestUninstall(t *testing.T) {
	dest := t.TempDir()
	// 两个版本(不同版本号目录)。
	for _, ver := range []string{"0.1.0", "0.2.0"} {
		src := t.TempDir()
		manifest := `{"id":"un-demo","version":"` + ver + `","kind":"go"}`
		if err := os.WriteFile(filepath.Join(src, "plugin.json"), []byte(manifest), 0o644); err != nil {
			t.Fatalf("写清单失败: %v", err)
		}
		if _, err := install.Install(src, dest, install.Options{}); err != nil {
			t.Fatalf("安装 %s 失败: %v", ver, err)
		}
	}
	entries, err := install.List(dest)
	if err != nil || len(entries) != 2 {
		t.Fatalf("安装后索引应有 2 条: %v %d", err, len(entries))
	}

	removed, err := install.Uninstall(dest, "un-demo")
	if err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if removed != 2 {
		t.Fatalf("应删除 2 个版本,实际 %d", removed)
	}
	entries, err = install.List(dest)
	if err != nil || len(entries) != 0 {
		t.Fatalf("卸载后索引应为空: %v %d", err, len(entries))
	}
	for _, ver := range []string{"0.1.0", "0.2.0"} {
		if _, err := os.Stat(filepath.Join(dest, "un-demo@"+ver)); err == nil {
			t.Fatalf("版本目录应被删除: %s", ver)
		}
	}
}

// 13. 卸载未知 id 报错。
func TestUninstallUnknown(t *testing.T) {
	if _, err := install.Uninstall(t.TempDir(), "nope"); err == nil {
		t.Fatalf("未安装的 id 应报错")
	}
}

// 14. 索引指向安装根之外的目录 → 拒绝删除。
func TestUninstallRejectsOutside(t *testing.T) {
	dest := t.TempDir()
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatalf("写标记失败: %v", err)
	}
	index := `{"entries":[{"id":"evil","version":"0.1.0","kind":"go","dir":"` + strings.ReplaceAll(outside, `\`, `\\`) + `","trust":"builtin"}]}`
	if err := os.WriteFile(filepath.Join(dest, install.IndexFile), []byte(index), 0o644); err != nil {
		t.Fatalf("写索引失败: %v", err)
	}
	if _, err := install.Uninstall(dest, "evil"); err == nil {
		t.Fatalf("安装根之外的目录应拒绝删除")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("外部文件不应被删除: %v", err)
	}
}

func installVersion(t *testing.T, dest, version string) {
	t.Helper()
	src := t.TempDir()
	manifest := `{"id":"up-demo","version":"` + version + `","kind":"go"}`
	if err := os.WriteFile(filepath.Join(src, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	if _, err := install.Install(src, dest, install.Options{}); err != nil {
		t.Fatalf("安装 %s 失败: %v", version, err)
	}
}

func upgradeVersion(t *testing.T, dest, version string) (*install.Info, error) {
	t.Helper()
	src := t.TempDir()
	manifest := `{"id":"up-demo","version":"` + version + `","kind":"go"}`
	if err := os.WriteFile(filepath.Join(src, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	return install.Upgrade(src, dest, install.Options{})
}

// 15. 升级:安装新版本并清理旧版本,索引只保留最新一条。
func TestUpgrade(t *testing.T) {
	dest := t.TempDir()
	installVersion(t, dest, "0.1.0")
	info, err := upgradeVersion(t, dest, "0.2.0")
	if err != nil {
		t.Fatalf("升级失败: %v", err)
	}
	if info.Version != "0.2.0" {
		t.Fatalf("升级版本不正确: %s", info.Version)
	}
	entries, err := install.List(dest)
	if err != nil || len(entries) != 1 || entries[0].Version != "0.2.0" {
		t.Fatalf("升级后索引应只剩最新: %v %+v", err, entries)
	}
	if _, err := os.Stat(filepath.Join(dest, "up-demo@0.1.0")); err == nil {
		t.Fatalf("旧版本目录应被删除")
	}
}

// 16. 升级拒绝降级与同版本;未安装时报错。
func TestUpgradeRejects(t *testing.T) {
	dest := t.TempDir()
	if _, err := upgradeVersion(t, dest, "0.1.0"); err == nil || !strings.Contains(err.Error(), "未安装") {
		t.Fatalf("未安装应报错,实际 %v", err)
	}
	installVersion(t, dest, "0.2.0")
	if _, err := upgradeVersion(t, dest, "0.2.0"); err == nil || !strings.Contains(err.Error(), "无需升级") {
		t.Fatalf("同版本应拒绝,实际 %v", err)
	}
	if _, err := upgradeVersion(t, dest, "0.1.0"); err == nil || !strings.Contains(err.Error(), "拒绝降级") {
		t.Fatalf("降级应拒绝,实际 %v", err)
	}
}

func upgradeVersionWith(t *testing.T, dest, version string, opts install.Options) (*install.Info, error) {
	t.Helper()
	src := t.TempDir()
	manifest := `{"id":"rb-demo","version":"` + version + `","kind":"go"}`
	if err := os.WriteFile(filepath.Join(src, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	return install.Upgrade(src, dest, opts)
}

func installRollbackVersion(t *testing.T, dest, version string) {
	t.Helper()
	src := t.TempDir()
	manifest := `{"id":"rb-demo","version":"` + version + `","kind":"go"}`
	if err := os.WriteFile(filepath.Join(src, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	if _, err := install.Install(src, dest, install.Options{}); err != nil {
		t.Fatalf("安装 %s 失败: %v", version, err)
	}
}

// 17. KeepPrevious:升级保留旧版本与索引条目。
func TestUpgradeKeepPrevious(t *testing.T) {
	dest := t.TempDir()
	installRollbackVersion(t, dest, "0.1.0")
	if _, err := upgradeVersionWith(t, dest, "0.2.0", install.Options{KeepPrevious: true}); err != nil {
		t.Fatalf("升级失败: %v", err)
	}
	entries, err := install.List(dest)
	if err != nil || len(entries) != 2 {
		t.Fatalf("KeepPrevious 应保留两个版本: %v %+v", err, entries)
	}
	for _, ver := range []string{"0.1.0", "0.2.0"} {
		if _, err := os.Stat(filepath.Join(dest, "rb-demo@"+ver)); err != nil {
			t.Fatalf("版本目录应存在: %s %v", ver, err)
		}
	}
}

// 18. Rollback:删除最新版本,索引回退到次新。
func TestRollback(t *testing.T) {
	dest := t.TempDir()
	installRollbackVersion(t, dest, "0.1.0")
	if _, err := upgradeVersionWith(t, dest, "0.2.0", install.Options{KeepPrevious: true}); err != nil {
		t.Fatalf("升级失败: %v", err)
	}
	version, err := install.Rollback(dest, "rb-demo")
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if version != "0.1.0" {
		t.Fatalf("应回滚到 0.1.0,实际 %s", version)
	}
	entries, err := install.List(dest)
	if err != nil || len(entries) != 1 || entries[0].Version != "0.1.0" {
		t.Fatalf("回滚后索引应只剩 0.1.0: %v %+v", err, entries)
	}
	if _, err := os.Stat(filepath.Join(dest, "rb-demo@0.2.0")); err == nil {
		t.Fatalf("最新版本目录应被删除")
	}
}

// 19. Rollback:没有旧版本时报错。
func TestRollbackNoPrevious(t *testing.T) {
	dest := t.TempDir()
	installRollbackVersion(t, dest, "0.1.0")
	if _, err := install.Rollback(dest, "rb-demo"); err == nil || !strings.Contains(err.Error(), "没有可回滚") {
		t.Fatalf("单版本应拒绝回滚,实际 %v", err)
	}
}

func writeRepoPlugin(t *testing.T, repoRoot, id, ver string) (string, string) {
	t.Helper()
	rel := filepath.Join("plugins", id+"@"+ver)
	abs := filepath.Join(repoRoot, rel)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	manifest := `{"id":"` + id + `","version":"` + ver + `","kind":"go"}`
	if err := os.WriteFile(filepath.Join(abs, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	info, err := install.VerifyDir(abs, install.Options{})
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	return rel, info.Digest
}

func writeRepoIndex(t *testing.T, repoRoot string, entries []install.RepoEntry) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"entries": entries})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, install.RepoFile), data, 0o644); err != nil {
		t.Fatalf("写索引失败: %v", err)
	}
}

// 20. LoadRepo/Latest 按 semver 选最高版本。
func TestRepoLatest(t *testing.T) {
	repo := t.TempDir()
	writeRepoIndex(t, repo, []install.RepoEntry{
		{ID: "r-demo", Version: "0.1.0", Dir: "x"},
		{ID: "r-demo", Version: "10.0.0", Dir: "y"},
		{ID: "r-demo", Version: "0.2.0", Dir: "z"},
	})
	entries, err := install.LoadRepo(repo)
	if err != nil {
		t.Fatalf("读仓库失败: %v", err)
	}
	latest, ok, err := install.LatestRepoEntry(entries, "r-demo")
	if err != nil || !ok || latest.Version != "10.0.0" {
		t.Fatalf("最新版本不正确: %+v ok=%v err=%v", latest, ok, err)
	}
}

// 21. UpdateFromRepo:安装更高版本并清理旧版本。
func TestUpdateFromRepo(t *testing.T) {
	dest := t.TempDir()
	installVersion(t, dest, "0.1.0")
	repo := t.TempDir()
	rel, digest := writeRepoPlugin(t, repo, "up-demo", "0.2.0")
	writeRepoIndex(t, repo, []install.RepoEntry{{ID: "up-demo", Version: "0.2.0", Dir: rel, Digest: digest}})

	info, err := install.UpdateFromRepo(repo, "up-demo", dest, install.Options{})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if info.Version != "0.2.0" {
		t.Fatalf("更新版本不正确: %s", info.Version)
	}
	entries, err := install.List(dest)
	if err != nil || len(entries) != 1 || entries[0].Version != "0.2.0" {
		t.Fatalf("更新后索引应只剩 0.2.0: %v %+v", err, entries)
	}
	if _, err := os.Stat(filepath.Join(dest, "up-demo@0.1.0")); err == nil {
		t.Fatalf("旧版本应被清理")
	}
}

// 22. 已是最新/摘要不匹配/目录逃逸都拒绝。
func TestUpdateFromRepoRejects(t *testing.T) {
	dest := t.TempDir()
	installVersion(t, dest, "0.2.0")
	repo := t.TempDir()
	rel, digest := writeRepoPlugin(t, repo, "up-demo", "0.2.0")
	writeRepoIndex(t, repo, []install.RepoEntry{{ID: "up-demo", Version: "0.2.0", Dir: rel, Digest: digest}})
	if _, err := install.UpdateFromRepo(repo, "up-demo", dest, install.Options{}); err == nil || !strings.Contains(err.Error(), "最新") {
		t.Fatalf("同版本应报最新,实际 %v", err)
	}

	rel3, digest3 := writeRepoPlugin(t, repo, "up-demo", "0.3.0")
	writeRepoIndex(t, repo, []install.RepoEntry{{ID: "up-demo", Version: "0.3.0", Dir: rel3, Digest: "bad-" + digest3}})
	if _, err := install.UpdateFromRepo(repo, "up-demo", dest, install.Options{}); err == nil || !strings.Contains(err.Error(), "摘要不匹配") {
		t.Fatalf("摘要不匹配应报错,实际 %v", err)
	}

	writeRepoIndex(t, repo, []install.RepoEntry{{ID: "up-demo", Version: "0.3.0", Dir: "../outside"}})
	if _, err := install.UpdateFromRepo(repo, "up-demo", dest, install.Options{}); err == nil || !strings.Contains(err.Error(), "逃逸") {
		t.Fatalf("目录逃逸应报错,实际 %v", err)
	}
}

// 23. 仓库签名:往返、篡改拒绝、缺签名按 requireRepo 处理。
func TestRepoSignature(t *testing.T) {
	keys, priv := newKeyRing(t, caps.TrustThirdParty)
	repo := t.TempDir()
	rel, digest := writeRepoPlugin(t, repo, "sig-demo", "0.2.0")
	entries := []install.RepoEntry{{ID: "sig-demo", Version: "0.2.0", Dir: rel, Digest: digest}}
	data, err := json.Marshal(map[string]any{"entries": entries})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, install.RepoFile), data, 0o644); err != nil {
		t.Fatalf("写索引失败: %v", err)
	}

	if _, err := install.LoadRepoVerified(repo, keys, true); err == nil || !strings.Contains(err.Error(), "缺少签名") {
		t.Fatalf("requireRepo 时缺签名应拒绝,实际 %v", err)
	}
	if _, err := install.LoadRepoVerified(repo, keys, false); err != nil {
		t.Fatalf("不要求签名时应允许: %v", err)
	}

	sigBytes := install.SignRepo(priv, data)
	sig := install.Signature{KeyID: "k1", Algo: "ed25519", Signature: base64.StdEncoding.EncodeToString(sigBytes)}
	sigData, _ := json.Marshal(sig)
	if err := os.WriteFile(filepath.Join(repo, install.RepoSignatureFile), sigData, 0o644); err != nil {
		t.Fatalf("写签名失败: %v", err)
	}
	if _, err := install.LoadRepoVerified(repo, keys, true); err != nil {
		t.Fatalf("签名应通过: %v", err)
	}

	// 篡改索引 → 验签失败。
	tampered := bytes.Replace(data, []byte("0.2.0"), []byte("0.3.0"), 1)
	if err := os.WriteFile(filepath.Join(repo, install.RepoFile), tampered, 0o644); err != nil {
		t.Fatalf("篡改失败: %v", err)
	}
	if _, err := install.LoadRepoVerified(repo, keys, true); err == nil {
		t.Fatalf("篡改索引应验签失败")
	}
}

// 24. 签名仓库上的 UpdateFromRepo 正常更新。
func TestUpdateFromSignedRepo(t *testing.T) {
	keys, priv := newKeyRing(t, caps.TrustThirdParty)
	dest := t.TempDir()
	installVersion(t, dest, "0.1.0")
	repo := t.TempDir()
	rel, digest := writeRepoPlugin(t, repo, "up-demo", "0.2.0")
	data, _ := json.Marshal(map[string]any{"entries": []install.RepoEntry{
		{ID: "up-demo", Version: "0.2.0", Dir: rel, Digest: digest},
	}})
	if err := os.WriteFile(filepath.Join(repo, install.RepoFile), data, 0o644); err != nil {
		t.Fatalf("写索引失败: %v", err)
	}
	sig := install.Signature{KeyID: "k1", Algo: "ed25519", Signature: base64.StdEncoding.EncodeToString(install.SignRepo(priv, data))}
	sigData, _ := json.Marshal(sig)
	if err := os.WriteFile(filepath.Join(repo, install.RepoSignatureFile), sigData, 0o644); err != nil {
		t.Fatalf("写签名失败: %v", err)
	}
	info, err := install.UpdateFromRepo(repo, "up-demo", dest, install.Options{RepoKeys: keys, RequireRepoSignature: true})
	if err != nil {
		t.Fatalf("签名仓库更新失败: %v", err)
	}
	if info.Version != "0.2.0" {
		t.Fatalf("版本不正确: %s", info.Version)
	}
}

// 25. 在线索引:未签名可读;require 时缺签名拒绝。
func TestFetchRepoIndex(t *testing.T) {
	data := []byte(`{"entries":[{"id":"r-demo","version":"0.1.0","dir":"plugins/r-demo@0.1.0"}]}`)
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) })
	server := httptest.NewServer(mux)
	defer server.Close()

	entries, err := install.FetchRepoIndex(context.Background(), server.URL+"/repo.json", nil, false)
	if err != nil || len(entries) != 1 || entries[0].ID != "r-demo" {
		t.Fatalf("未签名索引读取失败: %v %+v", err, entries)
	}
	if _, err := install.FetchRepoIndex(context.Background(), server.URL+"/repo.json", nil, true); err == nil {
		t.Fatalf("require 缺签名应拒绝")
	}
}

// 26. 在线索引验签:签名通过/篡改拒绝。
func TestFetchRepoIndexSigned(t *testing.T) {
	keys, priv := newKeyRing(t, caps.TrustThirdParty)
	data := []byte(`{"entries":[{"id":"r-demo","version":"0.2.0","dir":"plugins/r-demo@0.2.0"}]}`)
	sig := install.Signature{KeyID: "k1", Algo: "ed25519", Signature: base64.StdEncoding.EncodeToString(install.SignRepo(priv, data))}
	sigData, _ := json.Marshal(sig)
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) })
	mux.HandleFunc("/repo.sig.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sigData) })
	server := httptest.NewServer(mux)
	defer server.Close()

	if _, err := install.FetchRepoIndex(context.Background(), server.URL+"/repo.json", keys, true); err != nil {
		t.Fatalf("签名索引应通过: %v", err)
	}

	tampered := []byte(`{"entries":[{"id":"r-demo","version":"9.9.9","dir":"plugins/r-demo@9.9.9"}]}`)
	tamperMux := http.NewServeMux()
	tamperMux.HandleFunc("/repo.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tampered) })
	tamperMux.HandleFunc("/repo.sig.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sigData) })
	tamperSrv := httptest.NewServer(tamperMux)
	defer tamperSrv.Close()
	if _, err := install.FetchRepoIndex(context.Background(), tamperSrv.URL+"/repo.json", keys, true); err == nil {
		t.Fatalf("篡改索引应验签失败")
	}
}

func buildPluginZip(t *testing.T, id, ver string, prefix string) ([]byte, string) {
	t.Helper()
	src := t.TempDir()
	manifest := `{"id":"` + id + `","version":"` + ver + `","kind":"go"}`
	if err := os.WriteFile(filepath.Join(src, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	info, err := install.VerifyDir(src, install.Options{})
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entry := prefix + "plugin.json"
	_ = zw.SetComment("")
	f, err := zw.Create(entry)
	if err != nil {
		t.Fatalf("创建 zip 条目失败: %v", err)
	}
	if _, err := f.Write([]byte(manifest)); err != nil {
		t.Fatalf("写 zip 失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	return buf.Bytes(), info.Digest
}

// 27. 下载+解包+校验。
func TestFetchArtifactExtract(t *testing.T) {
	data, _ := buildPluginZip(t, "art-demo", "0.1.0", "pkg/")
	mux := http.NewServeMux()
	mux.HandleFunc("/p.zip", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	zipData, err := install.FetchArtifact(context.Background(), srv.URL+"/p.zip")
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	dir := t.TempDir()
	if err := install.ExtractZip(zipData, dir); err != nil {
		t.Fatalf("解包失败: %v", err)
	}
	info, err := install.VerifyDir(filepath.Join(dir, "pkg"), install.Options{})
	if err != nil || info.ID != "art-demo" {
		t.Fatalf("解包结果校验失败: %v %+v", err, info)
	}
}

// 28. zip slip 与非法 scheme 拒绝。
func TestArtifactRejects(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("../evil.txt")
	_, _ = f.Write([]byte("x"))
	_ = zw.Close()
	if err := install.ExtractZip(buf.Bytes(), t.TempDir()); err == nil {
		t.Fatalf("路径逃逸应拒绝")
	}
	if _, err := install.FetchArtifact(context.Background(), "ftp://example.com/p.zip"); err == nil {
		t.Fatalf("非法 scheme 应拒绝")
	}
}

// 29. 在线更新:索引 → artifact → 安装。
func TestUpdateFromRemote(t *testing.T) {
	dest := t.TempDir()
	installVersion(t, dest, "0.1.0")
	data, digest := buildPluginZip(t, "up-demo", "0.2.0", "pkg/")
	var serverURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/p.zip", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) })
	mux.HandleFunc("/repo.json", func(w http.ResponseWriter, _ *http.Request) {
		index := map[string]any{"entries": []install.RepoEntry{
			{ID: "up-demo", Version: "0.2.0", Source: serverURL + "/p.zip", Digest: digest},
		}}
		b, _ := json.Marshal(index)
		_, _ = w.Write(b)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	serverURL = srv.URL

	info, err := install.UpdateFromRemote(context.Background(), srv.URL+"/repo.json", "up-demo", dest, install.Options{})
	if err != nil {
		t.Fatalf("在线更新失败: %v", err)
	}
	if info.Version != "0.2.0" {
		t.Fatalf("版本不正确: %s", info.Version)
	}
	if _, err := os.Stat(filepath.Join(dest, "up-demo@0.1.0")); err == nil {
		t.Fatalf("旧版本应被清理")
	}
	if _, err := install.UpdateFromRemote(context.Background(), srv.URL+"/repo.json", "up-demo", dest, install.Options{}); err == nil {
		t.Fatalf("已是最新应失败")
	}
}

// 30. 安装锁:被占用时拒绝,正常结束后锁清理。
func TestInstallLock(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "plugin.json"),
		[]byte(`{"id":"lock-demo","version":"0.1.0","kind":"go"}`), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	dest := t.TempDir()
	lock := filepath.Join(dest, install.LockFile)
	if err := os.WriteFile(lock, []byte("held"), 0o644); err != nil {
		t.Fatalf("写锁失败: %v", err)
	}
	if _, err := install.Install(src, dest, install.Options{}); err == nil || !strings.Contains(err.Error(), "正在被另一个安装进程") {
		t.Fatalf("锁被占用应拒绝,实际 %v", err)
	}
	_ = os.Remove(lock)

	if _, err := install.Install(src, dest, install.Options{}); err != nil {
		t.Fatalf("释放锁后应安装成功: %v", err)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Fatalf("安装结束后锁文件应被删除")
	}
}

// 31. 过期锁(崩溃残留)自动回收后重试。
func TestInstallLockStale(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "plugin.json"),
		[]byte(`{"id":"stale-demo","version":"0.1.0","kind":"go"}`), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	dest := t.TempDir()
	lock := filepath.Join(dest, install.LockFile)
	if err := os.WriteFile(lock, []byte("stale"), 0o644); err != nil {
		t.Fatalf("写锁失败: %v", err)
	}
	old := time.Now().Add(-2 * install.InstallLockTTL)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatalf("改锁时间失败: %v", err)
	}
	if _, err := install.Install(src, dest, install.Options{}); err != nil {
		t.Fatalf("过期锁应被回收并安装成功: %v", err)
	}
}

// 32. 已安装目录按 index 的 trust 加载,而不是退回 third-party。
func TestLoadDirUsesInstallTrust(t *testing.T) {
	dir, raw, digest := writeTSPlugin(t)
	keys, priv := newKeyRing(t, caps.TrustVerified)
	writeSignature(t, dir, "k1", install.Sign(priv, raw, digest))
	dest := t.TempDir()
	if _, err := install.Install(dir, dest, install.Options{Keys: keys}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	ms, err := install.LoadDir(dest)
	if err != nil {
		t.Fatalf("加载安装目录失败: %v", err)
	}
	if len(ms) != 1 || ms[0].ID != "ts-demo" {
		t.Fatalf("加载结果不正确: %+v", ms)
	}
	if ms[0].Trust != string(caps.TrustVerified) {
		t.Fatalf("trust 应来自索引 verified,实际 %q", ms[0].Trust)
	}
}
