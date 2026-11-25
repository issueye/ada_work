// 在线仓库索引:HTTP 读取 repo.json(可选验签),不做插件下载。
package install

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RepoFetchLimit 是仓库索引的最大字节数(1 MiB)。
const RepoFetchLimit int64 = 1 << 20

// RepoFetchTimeout 是在线读取索引的超时。
const RepoFetchTimeout = 20 * time.Second

// FetchRepoIndex 从 URL 读取仓库索引,并按需校验签名。
//
// 签名地址约定:与索引同目录的 repo.sig.json(若 url 不以 repo.json 结尾则追加 .sig)。
// 超时/非 200/超过大小上限都返回错误。
func FetchRepoIndex(ctx context.Context, url string, keys map[string]Key, requireRepo bool) ([]RepoEntry, error) {
	index, err := fetchBytes(ctx, url)
	if err != nil {
		return nil, err
	}
	sigURL := repoSignatureURL(url)
	sigData, status, err := fetchBytesStatus(ctx, sigURL)
	switch {
	case err != nil:
		return nil, err
	case status == http.StatusNotFound:
		if requireRepo {
			return nil, fmt.Errorf("install: 在线仓库缺少签名 %s", sigURL)
		}
	case status != http.StatusOK:
		return nil, fmt.Errorf("install: 读取仓库签名失败: HTTP %d", status)
	default:
		var sig Signature
		if err := json.Unmarshal(sigData, &sig); err != nil {
			return nil, fmt.Errorf("install: 仓库签名损坏: %w", err)
		}
		key, ok := keys[sig.KeyID]
		if !ok {
			return nil, fmt.Errorf("install: 仓库签名的 keyId %q 不在 keyring", sig.KeyID)
		}
		rawSig, err := base64.StdEncoding.DecodeString(sig.Signature)
		if err != nil {
			return nil, fmt.Errorf("install: 仓库签名不是合法 base64: %w", err)
		}
		if err := VerifyRepoSignature(key.Public, index, rawSig); err != nil {
			return nil, err
		}
	}
	return parseRepoIndex(index)
}

// repoSignatureURL 把 repo.json 的 URL 映射到 repo.sig.json。
func repoSignatureURL(u string) string {
	base := u
	if i := strings.LastIndex(base, "?"); i >= 0 {
		base = base[:i]
	}
	if strings.HasSuffix(base, "repo.json") {
		return strings.TrimSuffix(base, "repo.json") + "repo.sig.json"
	}
	return base + ".sig"
}

func fetchBytes(ctx context.Context, url string) ([]byte, error) {
	data, _, err := fetchBytesStatus(ctx, url)
	return data, err
}

func fetchBytesStatus(ctx context.Context, url string) ([]byte, int, error) {
	actx, cancel := context.WithTimeout(ctx, RepoFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("install: 构造请求失败: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("install: 读取 %s 失败: %w", url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, RepoFetchLimit+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("install: 读取 %s 失败: %w", url, err)
	}
	if int64(len(data)) > RepoFetchLimit {
		return nil, resp.StatusCode, fmt.Errorf("install: %s 超过 %d 字节上限", url, RepoFetchLimit)
	}
	return data, resp.StatusCode, nil
}
