// Package jsrt 的 node:crypto 宿主能力:摘要、HMAC、PBKDF2、随机数与定长比较。
//
// 放在执行层的理由:这些能力没有策略语义(不读文件、不发请求、不改进程状态),
// 与 node:path / node:os 同属「默认可用、无需授权的环境能力」。
//
// 边界:跨边界只传 base64 字符串(不传二进制对象),单次输入上限 8 MiB;
// 算法白名单固定在 md5/sha1/sha256/sha384/sha512,未知算法在 JS 侧就被拒绝,
// 不会走到宿主的映射表里。
package jsrt

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"hash"
	"strings"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

const (
	// maxRandomBytesSize 与 node:crypto.randomBytes 的模块侧上限一致。
	maxRandomBytesSize = 65536
	// maxCryptoInput 是单次摘要/HMAC/PBKDF2 的输入上限(解码后的字节数)。
	maxCryptoInput = 8 << 20
	// maxPBKDF2Iterations 防止一次调用把事件循环按在 CPU 上太久。
	maxPBKDF2Iterations = 10_000_000
	// maxPBKDF2KeyLen 是派生密钥长度上限。
	maxPBKDF2KeyLen = 1024
)

// cryptoHasher 按算法名返回构造函数;第二个返回值表示是否支持。
func cryptoHasher(alg string) (func() hash.Hash, bool) {
	switch strings.ToLower(alg) {
	case "md5":
		return md5.New, true
	case "sha1":
		return sha1.New, true
	case "sha256":
		return sha256.New, true
	case "sha384":
		return sha512.New384, true
	case "sha512":
		return sha512.New, true
	}
	return nil, false
}

// base64Arg 取第 i 个参数并解码 base64。
func base64Arg(raw *moejs.Runtime, args []engine.Value, i int, name string) ([]byte, error) {
	if i >= len(args) {
		return nil, fmt.Errorf("%s 缺失", name)
	}
	text, err := toString(raw, args[i])
	if err != nil {
		return nil, fmt.Errorf("%s 必须是 base64 字符串: %w", name, err)
	}
	out, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("%s 不是合法的 base64: %w", name, err)
	}
	if len(out) > maxCryptoInput {
		return nil, fmt.Errorf("%s 超过 %d 字节上限", name, maxCryptoInput)
	}
	return out, nil
}

// intArg 取第 i 个参数并转成 int64。
func intArg(raw *moejs.Runtime, args []engine.Value, i int, name string) (int64, error) {
	if i >= len(args) {
		return 0, fmt.Errorf("%s 缺失", name)
	}
	v, err := raw.ToGo(args[i])
	if err != nil {
		return 0, fmt.Errorf("%s 无法转换: %w", name, err)
	}
	n, ok := toInt64(v)
	if !ok {
		return 0, fmt.Errorf("%s 必须是整数", name)
	}
	return n, nil
}

// nativeCryptoHash 实现 __adaHost.crypto.hash(alg, dataB64):返回摘要的 base64。
func (r *Runtime) nativeCryptoHash(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("crypto.hash(alg, dataB64) 需要算法名")
		}
		alg, err := toString(raw, args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("算法名必须是字符串")
		}
		newHash, ok := cryptoHasher(alg)
		if !ok {
			return engine.Undefined(), fmt.Errorf("不支持的摘要算法: %s", alg)
		}
		data, err := base64Arg(raw, args, 1, "data")
		if err != nil {
			return engine.Undefined(), err
		}
		h := newHash()
		h.Write(data)
		return raw.FromGo(base64.StdEncoding.EncodeToString(h.Sum(nil)))
	}
}

// nativeCryptoHMAC 实现 __adaHost.crypto.hmac(alg, keyB64, dataB64)。
func (r *Runtime) nativeCryptoHMAC(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("crypto.hmac(alg, keyB64, dataB64) 需要算法名")
		}
		alg, err := toString(raw, args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("算法名必须是字符串")
		}
		newHash, ok := cryptoHasher(alg)
		if !ok {
			return engine.Undefined(), fmt.Errorf("不支持的 HMAC 算法: %s", alg)
		}
		key, err := base64Arg(raw, args, 1, "key")
		if err != nil {
			return engine.Undefined(), err
		}
		data, err := base64Arg(raw, args, 2, "data")
		if err != nil {
			return engine.Undefined(), err
		}
		mac := hmac.New(newHash, key)
		mac.Write(data)
		return raw.FromGo(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}
}

// nativeCryptoPBKDF2 实现 __adaHost.crypto.pbkdf2(passB64, saltB64, iter, keyLen, alg),同步版本。
func (r *Runtime) nativeCryptoPBKDF2(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		out, err := cryptoPBKDF2Args(raw, args)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

// nativeCryptoPBKDF2Async 是 PBKDF2 的 Promise 版本。
//
// PBKDF2 是 CPU 密集的:同步跑会把事件循环按住(几十万次迭代就是几百毫秒),
// 所以这里在 goroutine 里算,结算时回循环——与 node:fs 的异步形状同一套机制。
func (r *Runtime) nativeCryptoPBKDF2Async(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		// 参数先在循环上取好(engine.Value 不能跨 goroutine),再交给后台计算。
		pass, err := base64Arg(raw, args, 0, "password")
		if err != nil {
			return engine.Undefined(), err
		}
		salt, err := base64Arg(raw, args, 1, "salt")
		if err != nil {
			return engine.Undefined(), err
		}
		iter, keyLen, alg, err := cryptoPBKDF2Params(raw, args)
		if err != nil {
			return engine.Undefined(), err
		}
		r.mu.Lock()
		loop := r.loop
		r.mu.Unlock()
		if loop == nil {
			return engine.Undefined(), fmt.Errorf("crypto: 事件循环不可用")
		}
		p, settle := loop.NewPromise()
		go func() {
			out, calcErr := cryptoPBKDF2(pass, salt, iter, keyLen, alg)
			settle(func(rt *moejs.Runtime) (engine.Value, error) {
				if calcErr != nil {
					return engine.Undefined(), calcErr
				}
				return rt.FromGo(out)
			})
		}()
		return p, nil
	}
}

// cryptoPBKDF2Args 解出参数并计算(同步路径)。
func cryptoPBKDF2Args(raw *moejs.Runtime, args []engine.Value) (string, error) {
	pass, err := base64Arg(raw, args, 0, "password")
	if err != nil {
		return "", err
	}
	salt, err := base64Arg(raw, args, 1, "salt")
	if err != nil {
		return "", err
	}
	iter, keyLen, alg, err := cryptoPBKDF2Params(raw, args)
	if err != nil {
		return "", err
	}
	return cryptoPBKDF2(pass, salt, iter, keyLen, alg)
}

// cryptoPBKDF2Params 取出 iterations / keyLen / alg 并做范围校验。
func cryptoPBKDF2Params(raw *moejs.Runtime, args []engine.Value) (int, int, string, error) {
	iter, err := intArg(raw, args, 2, "iterations")
	if err != nil {
		return 0, 0, "", err
	}
	if iter < 1 || iter > maxPBKDF2Iterations {
		return 0, 0, "", fmt.Errorf("iterations 必须在 1..%d 之间", maxPBKDF2Iterations)
	}
	keyLen, err := intArg(raw, args, 3, "keylen")
	if err != nil {
		return 0, 0, "", err
	}
	if keyLen < 1 || keyLen > maxPBKDF2KeyLen {
		return 0, 0, "", fmt.Errorf("keylen 必须在 1..%d 之间", maxPBKDF2KeyLen)
	}
	alg := "sha1"
	if len(args) > 4 {
		if s, sErr := toString(raw, args[4]); sErr == nil && s != "" {
			alg = s
		}
	}
	if _, ok := cryptoHasher(alg); !ok {
		return 0, 0, "", fmt.Errorf("不支持的 PBKDF2 摘要算法: %s", alg)
	}
	return int(iter), int(keyLen), strings.ToLower(alg), nil
}

// cryptoPBKDF2 真正做派生,返回 base64;可在任意 goroutine 上调用。
func cryptoPBKDF2(pass, salt []byte, iter, keyLen int, alg string) (string, error) {
	// pbkdf2.Key 是泛型的,需要具体类型;这里按算法分派(白名单已校验过)。
	var (
		derived []byte
		err     error
	)
	switch alg {
	case "md5":
		derived, err = pbkdf2.Key(md5.New, string(pass), salt, iter, keyLen)
	case "sha1":
		derived, err = pbkdf2.Key(sha1.New, string(pass), salt, iter, keyLen)
	case "sha256":
		derived, err = pbkdf2.Key(sha256.New, string(pass), salt, iter, keyLen)
	case "sha384":
		derived, err = pbkdf2.Key(sha512.New384, string(pass), salt, iter, keyLen)
	case "sha512":
		derived, err = pbkdf2.Key(sha512.New, string(pass), salt, iter, keyLen)
	default:
		return "", fmt.Errorf("不支持的 PBKDF2 摘要算法: %s", alg)
	}
	if err != nil {
		return "", fmt.Errorf("PBKDF2 计算失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(derived), nil
}

// nativeCryptoRandomBytes 实现 __adaHost.crypto.randomBytes(n):返回 n 字节的 base64。
func (r *Runtime) nativeCryptoRandomBytes(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		n, err := intArg(raw, args, 0, "size")
		if err != nil {
			return engine.Undefined(), err
		}
		if n < 0 || n > maxRandomBytesSize {
			return engine.Undefined(), fmt.Errorf("randomBytes 的长度必须在 0..%d 之间", maxRandomBytesSize)
		}
		buf := make([]byte, n)
		if _, rerr := rand.Read(buf); rerr != nil {
			return engine.Undefined(), fmt.Errorf("randomBytes 生成失败: %w", rerr)
		}
		return raw.FromGo(base64.StdEncoding.EncodeToString(buf))
	}
}

// nativeCryptoTimingSafeEqual 实现 __adaHost.crypto.timingSafeEqual(aB64, bB64)。
//
// 长度不同直接抛错(与 Node 一致):长度本身不是秘密,提前返回不泄露内容。
func (r *Runtime) nativeCryptoTimingSafeEqual(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		a, err := base64Arg(raw, args, 0, "a")
		if err != nil {
			return engine.Undefined(), err
		}
		b, err := base64Arg(raw, args, 1, "b")
		if err != nil {
			return engine.Undefined(), err
		}
		if len(a) != len(b) {
			return engine.Undefined(), fmt.Errorf("timingSafeEqual 的两个输入长度必须相同")
		}
		return raw.FromGo(hmac.Equal(a, b))
	}
}
